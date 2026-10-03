package ec2

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Volume and attachment state values. kumo executes every mutation
// synchronously, so the transitional states (creating, attaching, detaching)
// are never observed.
const (
	volumeStateInUse         = "in-use"
	volumeAttachmentAttached = "attached"
	volumeAttachmentDetached = "detached"
)

// Volume type defaults.
const (
	volumeTypeGP2           = "gp2"
	volumeTypeGP3           = "gp3"
	defaultSnapshotSizeGiB  = 8
	gp2MinIops              = 100
	gp2MaxIops              = 16000
	gp2IopsPerGiB           = 3
	gp3BaselineIops         = 3000
	gp3BaselineThroughputMB = 125
)

// Error codes specific to volume operations.
const (
	errCodeVolumeInUse                 = "VolumeInUse"
	errCodeIncorrectState              = "IncorrectState"
	errCodeIncorrectInstanceState      = "IncorrectInstanceState"
	errCodeInvalidAttachmentNotFound   = "InvalidAttachment.NotFound"
	errCodeMissingParameter            = "MissingParameter"
	errCodeInvalidParameterCombination = "InvalidParameterCombination"
)

// CreateVolume creates a new EBS volume in the available state.
func (m *MemoryStorage) CreateVolume(_ context.Context, req *CreateVolumeRequest) (*Volume, error) {
	if req.Size <= 0 && req.SnapshotID == "" {
		return nil, &Error{
			Code:    errCodeMissingParameter,
			Message: "The request must contain the parameter size or snapshotId",
		}
	}

	size := req.Size
	if size <= 0 {
		size = defaultSnapshotSizeGiB
	}

	volumeType := req.VolumeType
	if volumeType == "" {
		volumeType = volumeTypeGP2
	}

	iops, throughput := req.Iops, req.Throughput

	switch volumeType {
	case volumeTypeGP2:
		iops = min(max(size*gp2IopsPerGiB, gp2MinIops), gp2MaxIops)
	case volumeTypeGP3:
		if iops == 0 {
			iops = gp3BaselineIops
		}

		if throughput == 0 {
			throughput = gp3BaselineThroughputMB
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	vol := &Volume{
		VolumeID:           "vol-" + generateID(),
		Size:               size,
		SnapshotID:         req.SnapshotID,
		AvailabilityZone:   req.AvailabilityZone,
		State:              stateAvailable,
		VolumeType:         volumeType,
		Iops:               iops,
		Throughput:         throughput,
		Encrypted:          req.Encrypted || req.KmsKeyID != "",
		KmsKeyID:           req.KmsKeyID,
		MultiAttachEnabled: req.MultiAttachEnabled,
		CreateTime:         time.Now().UTC(),
		Attachments:        []VolumeAttachment{},
		Tags:               []Tag{},
	}

	m.Volumes[vol.VolumeID] = vol

	m.saveLocked()

	return vol, nil
}

// DeleteVolume deletes an EBS volume. Attached volumes cannot be deleted.
func (m *MemoryStorage) DeleteVolume(_ context.Context, volumeID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vol, err := m.findVolumeLocked(volumeID)
	if err != nil {
		return err
	}

	if len(vol.Attachments) > 0 {
		return &Error{
			Code:    errCodeVolumeInUse,
			Message: fmt.Sprintf("Volume %s is currently attached to %s", volumeID, vol.Attachments[0].InstanceID),
		}
	}

	delete(m.Volumes, volumeID)

	m.saveLocked()

	return nil
}

// AttachVolume attaches an EBS volume to an instance at the given device.
func (m *MemoryStorage) AttachVolume(_ context.Context, req *AttachVolumeRequest) (*VolumeAttachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vol, err := m.findVolumeLocked(req.VolumeID)
	if err != nil {
		return nil, err
	}

	instance, exists := m.Instances[req.InstanceID]
	if !exists {
		return nil, &Error{
			Code:    errCodeInvalidInstanceIDNotFound,
			Message: fmt.Sprintf("The instance ID '%s' does not exist", req.InstanceID),
		}
	}

	if instance.State.Name == InstanceStateNameTerminated || instance.State.Name == InstanceStateNameShuttingDown {
		return nil, &Error{
			Code:    errCodeIncorrectInstanceState,
			Message: fmt.Sprintf("The instance '%s' is not in a valid state for this operation", req.InstanceID),
		}
	}

	if err := checkVolumeAttachable(vol, req.InstanceID); err != nil {
		return nil, err
	}

	if m.deviceInUseLocked(req.InstanceID, req.Device) {
		return nil, &Error{
			Code:    errInvalidParameter,
			Message: fmt.Sprintf("Attachment point %s is already in use", req.Device),
		}
	}

	attachment := VolumeAttachment{
		VolumeID:   vol.VolumeID,
		InstanceID: req.InstanceID,
		Device:     req.Device,
		State:      volumeAttachmentAttached,
		AttachTime: time.Now().UTC(),
	}

	vol.Attachments = append(vol.Attachments, attachment)
	vol.State = volumeStateInUse

	m.saveLocked()

	return &attachment, nil
}

// checkVolumeAttachable reports whether vol can take another attachment to
// instanceID: a regular volume must be unattached, and a multi-attach volume
// must not already be attached to the same instance.
func checkVolumeAttachable(vol *Volume, instanceID string) error {
	if len(vol.Attachments) == 0 {
		return nil
	}

	if !vol.MultiAttachEnabled {
		return &Error{
			Code:    errCodeVolumeInUse,
			Message: fmt.Sprintf("%s is already attached to an instance", vol.VolumeID),
		}
	}

	for _, a := range vol.Attachments {
		if a.InstanceID == instanceID {
			return &Error{
				Code:    errCodeVolumeInUse,
				Message: fmt.Sprintf("%s is already attached to instance %s", vol.VolumeID, instanceID),
			}
		}
	}

	return nil
}

// deviceInUseLocked reports whether any volume is attached to instanceID at
// device. The caller must hold the lock.
func (m *MemoryStorage) deviceInUseLocked(instanceID, device string) bool {
	for _, v := range m.Volumes {
		for _, a := range v.Attachments {
			if a.InstanceID == instanceID && a.Device == device {
				return true
			}
		}
	}

	return false
}

// DetachVolume detaches an EBS volume. InstanceID and Device are optional
// but, when given, must match the attachment being removed.
func (m *MemoryStorage) DetachVolume(_ context.Context, req *DetachVolumeRequest) (*VolumeAttachment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vol, err := m.findVolumeLocked(req.VolumeID)
	if err != nil {
		return nil, err
	}

	if len(vol.Attachments) == 0 {
		return nil, &Error{
			Code:    errCodeIncorrectState,
			Message: fmt.Sprintf("Volume '%s' is in the '%s' state.", vol.VolumeID, vol.State),
		}
	}

	if req.InstanceID == "" && len(vol.Attachments) > 1 {
		return nil, &Error{
			Code:    errCodeInvalidParameterCombination,
			Message: fmt.Sprintf("Volume '%s' is attached to multiple instances; InstanceId is required", vol.VolumeID),
		}
	}

	idx := slices.IndexFunc(vol.Attachments, func(a VolumeAttachment) bool {
		return (req.InstanceID == "" || a.InstanceID == req.InstanceID) &&
			(req.Device == "" || a.Device == req.Device)
	})
	if idx < 0 {
		return nil, &Error{
			Code:    errCodeInvalidAttachmentNotFound,
			Message: fmt.Sprintf("Volume '%s' is not attached to instance '%s' at '%s'", vol.VolumeID, req.InstanceID, req.Device),
		}
	}

	detached := vol.Attachments[idx]
	detached.State = volumeAttachmentDetached

	vol.Attachments = slices.Delete(vol.Attachments, idx, idx+1)
	if len(vol.Attachments) == 0 {
		vol.State = stateAvailable
	}

	m.saveLocked()

	return &detached, nil
}

// DescribeVolumes returns the requested volumes, or all volumes when
// volumeIDs is empty, narrowed by the supported filters and sorted by ID.
func (m *MemoryStorage) DescribeVolumes(_ context.Context, volumeIDs []string, filters map[string][]string) ([]*Volume, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	candidates := make([]*Volume, 0, len(m.Volumes))

	if len(volumeIDs) == 0 {
		for _, v := range m.Volumes {
			candidates = append(candidates, v)
		}
	} else {
		for _, id := range volumeIDs {
			v, err := m.findVolumeLocked(id)
			if err != nil {
				return nil, err
			}

			candidates = append(candidates, v)
		}
	}

	out := make([]*Volume, 0, len(candidates))

	for _, v := range candidates {
		if matchVolumeFilters(v, filters) {
			out = append(out, cloneVolume(v))
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].VolumeID < out[j].VolumeID })

	return out, nil
}

// releaseInstanceVolumesLocked drops every attachment to instanceID, deleting
// the volumes flagged DeleteOnTermination. Called when an instance terminates
// so its volumes do not stay in-use forever. The caller must hold the lock.
func (m *MemoryStorage) releaseInstanceVolumesLocked(instanceID string) {
	for id, v := range m.Volumes {
		deleteVolume := false

		v.Attachments = slices.DeleteFunc(v.Attachments, func(a VolumeAttachment) bool {
			if a.InstanceID != instanceID {
				return false
			}

			deleteVolume = deleteVolume || a.DeleteOnTermination

			return true
		})

		switch {
		case deleteVolume && len(v.Attachments) == 0:
			delete(m.Volumes, id)
		case len(v.Attachments) == 0:
			v.State = stateAvailable
		}
	}
}

// findVolumeLocked looks up a volume by ID. The caller must hold the lock.
func (m *MemoryStorage) findVolumeLocked(volumeID string) (*Volume, error) {
	vol, exists := m.Volumes[volumeID]
	if !exists {
		return nil, &Error{
			Code:    errCodeInvalidVolumeNotFound,
			Message: fmt.Sprintf("The volume '%s' does not exist.", volumeID),
		}
	}

	return vol, nil
}

// cloneVolume returns a copy of v whose slices do not alias the stored volume.
func cloneVolume(v *Volume) *Volume {
	c := *v
	c.Attachments = slices.Clone(v.Attachments)
	c.Tags = slices.Clone(v.Tags)

	return &c
}

// matchVolumeFilters reports whether v satisfies every filter. Values within
// one filter are ORed; distinct filters are ANDed. Unsupported filter names
// are accepted and ignored, matching DescribeTags.
func matchVolumeFilters(v *Volume, filters map[string][]string) bool {
	for name, values := range filters {
		if len(values) == 0 {
			continue
		}

		if !matchVolumeFilter(v, name, values) {
			return false
		}
	}

	return true
}

func matchVolumeFilter(v *Volume, name string, values []string) bool {
	if key, ok := strings.CutPrefix(name, "tag:"); ok {
		return slices.ContainsFunc(v.Tags, func(t Tag) bool {
			return t.Key == key && slices.Contains(values, t.Value)
		})
	}

	if field, ok := strings.CutPrefix(name, "attachment."); ok {
		for i := range v.Attachments {
			if matchVolumeAttachmentFilter(&v.Attachments[i], field, values) {
				return true
			}
		}

		return false
	}

	if name == "tag-key" {
		return slices.ContainsFunc(v.Tags, func(t Tag) bool { return slices.Contains(values, t.Key) })
	}

	field, ok := volumeFilterFields[name]
	if !ok {
		return true
	}

	return slices.Contains(values, field(v))
}

// volumeFilterFields maps scalar DescribeVolumes filter names to the volume
// field they compare against.
var volumeFilterFields = map[string]func(*Volume) string{
	"volume-id":            func(v *Volume) string { return v.VolumeID },
	"status":               func(v *Volume) string { return v.State },
	"availability-zone":    func(v *Volume) string { return v.AvailabilityZone },
	"volume-type":          func(v *Volume) string { return v.VolumeType },
	"size":                 func(v *Volume) string { return strconv.Itoa(v.Size) },
	"snapshot-id":          func(v *Volume) string { return v.SnapshotID },
	"encrypted":            func(v *Volume) string { return strconv.FormatBool(v.Encrypted) },
	"multi-attach-enabled": func(v *Volume) string { return strconv.FormatBool(v.MultiAttachEnabled) },
}

func matchVolumeAttachmentFilter(a *VolumeAttachment, field string, values []string) bool {
	switch field {
	case "instance-id":
		return slices.Contains(values, a.InstanceID)
	case "device":
		return slices.Contains(values, a.Device)
	case "status":
		return slices.Contains(values, a.State)
	case "delete-on-termination":
		return slices.Contains(values, strconv.FormatBool(a.DeleteOnTermination))
	default:
		return true
	}
}
