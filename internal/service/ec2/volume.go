package ec2

import (
	"net/http"

	"github.com/google/uuid"
)

// ec2TimeFormat is the timestamp layout EC2 uses in XML responses.
const ec2TimeFormat = "2006-01-02T15:04:05.000Z"

// CreateVolume handles the CreateVolume action.
func (s *Service) CreateVolume(w http.ResponseWriter, r *http.Request) {
	var req CreateVolumeRequest
	if !decodeEC2Request(w, r, &req) {
		return
	}

	if !requireEC2Parameter(w, req.AvailabilityZone == "", "AvailabilityZone") {
		return
	}

	vol, err := s.storage.CreateVolume(r.Context(), &req)
	if err != nil {
		handleError(w, err)

		return
	}

	applyTagsOnCreate(r, s.storage, vol.VolumeID, "volume")

	writeEC2XMLResponse(w, XMLCreateVolumeResponse{
		Xmlns:     ec2XMLNS,
		RequestID: uuid.New().String(),
		XMLVolume: convertToXMLVolume(vol),
	})
}

// DeleteVolume handles the DeleteVolume action.
func (s *Service) DeleteVolume(w http.ResponseWriter, r *http.Request) {
	var req DeleteVolumeRequest
	if !decodeEC2Request(w, r, &req) {
		return
	}

	if !requireEC2Parameter(w, req.VolumeID == "", "VolumeId") {
		return
	}

	if err := s.storage.DeleteVolume(r.Context(), req.VolumeID); err != nil {
		handleError(w, err)

		return
	}

	writeEC2XMLResponse(w, XMLDeleteVolumeResponse{
		Xmlns:     ec2XMLNS,
		RequestID: uuid.New().String(),
		Return:    true,
	})
}

// AttachVolume handles the AttachVolume action.
func (s *Service) AttachVolume(w http.ResponseWriter, r *http.Request) {
	var req AttachVolumeRequest
	if !decodeEC2Request(w, r, &req) {
		return
	}

	if !requireEC2Parameter(w, req.VolumeID == "", "VolumeId") {
		return
	}

	if !requireEC2Parameter(w, req.InstanceID == "", "InstanceId") {
		return
	}

	if !requireEC2Parameter(w, req.Device == "", "Device") {
		return
	}

	attachment, err := s.storage.AttachVolume(r.Context(), &req)
	if err != nil {
		handleError(w, err)

		return
	}

	writeEC2XMLResponse(w, XMLAttachVolumeResponse{
		Xmlns:               ec2XMLNS,
		RequestID:           uuid.New().String(),
		XMLVolumeAttachment: convertToXMLVolumeAttachment(attachment),
	})
}

// DetachVolume handles the DetachVolume action.
func (s *Service) DetachVolume(w http.ResponseWriter, r *http.Request) {
	var req DetachVolumeRequest
	if !decodeEC2Request(w, r, &req) {
		return
	}

	if !requireEC2Parameter(w, req.VolumeID == "", "VolumeId") {
		return
	}

	attachment, err := s.storage.DetachVolume(r.Context(), &req)
	if err != nil {
		handleError(w, err)

		return
	}

	writeEC2XMLResponse(w, XMLDetachVolumeResponse{
		Xmlns:               ec2XMLNS,
		RequestID:           uuid.New().String(),
		XMLVolumeAttachment: convertToXMLVolumeAttachment(attachment),
	})
}

// DescribeVolumes handles the DescribeVolumes action.
func (s *Service) DescribeVolumes(w http.ResponseWriter, r *http.Request) {
	var req DescribeVolumesRequest
	if !decodeEC2Request(w, r, &req) {
		return
	}

	if !decodeEC2Form(w, r) {
		return
	}

	volumes, err := s.storage.DescribeVolumes(r.Context(), req.VolumeIDs, parseFiltersFromForm(r.Form))
	if err != nil {
		handleError(w, err)

		return
	}

	items := make([]XMLVolume, 0, len(volumes))
	for _, v := range volumes {
		items = append(items, convertToXMLVolume(v))
	}

	writeEC2XMLResponse(w, XMLDescribeVolumesResponse{
		Xmlns:     ec2XMLNS,
		RequestID: uuid.New().String(),
		VolumeSet: XMLVolumeSet{Items: items},
	})
}

// convertToXMLVolume converts a Volume to XMLVolume.
func convertToXMLVolume(v *Volume) XMLVolume {
	attachments := make([]XMLVolumeAttachment, 0, len(v.Attachments))
	for i := range v.Attachments {
		attachments = append(attachments, convertToXMLVolumeAttachment(&v.Attachments[i]))
	}

	tags := make([]XMLTag, 0, len(v.Tags))
	for _, t := range v.Tags {
		tags = append(tags, XMLTag(t))
	}

	return XMLVolume{
		VolumeID:           v.VolumeID,
		Size:               v.Size,
		SnapshotID:         v.SnapshotID,
		AvailabilityZone:   v.AvailabilityZone,
		Status:             v.State,
		CreateTime:         v.CreateTime.Format(ec2TimeFormat),
		AttachmentSet:      XMLVolumeAttachmentSet{Items: attachments},
		TagSet:             XMLTagSet{Items: tags},
		VolumeType:         v.VolumeType,
		Iops:               v.Iops,
		Throughput:         v.Throughput,
		Encrypted:          v.Encrypted,
		KmsKeyID:           v.KmsKeyID,
		MultiAttachEnabled: v.MultiAttachEnabled,
	}
}

// convertToXMLVolumeAttachment converts a VolumeAttachment to XMLVolumeAttachment.
func convertToXMLVolumeAttachment(a *VolumeAttachment) XMLVolumeAttachment {
	return XMLVolumeAttachment{
		VolumeID:            a.VolumeID,
		InstanceID:          a.InstanceID,
		Device:              a.Device,
		Status:              a.State,
		AttachTime:          a.AttachTime.Format(ec2TimeFormat),
		DeleteOnTermination: a.DeleteOnTermination,
	}
}
