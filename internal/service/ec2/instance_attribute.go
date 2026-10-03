package ec2

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"

	"github.com/google/uuid"
)

// Defaults reported for instance attributes kumo does not otherwise model.
// They are applied when rendering, so instances persisted before these
// attributes existed still describe like freshly launched ones.
const (
	defaultInstanceAvailabilityZone = "us-east-1a"
	defaultInstanceRootDeviceName   = "/dev/xvda"
	defaultShutdownBehavior         = "stop"
)

const (
	errMissingParameter           = "MissingParameter"
	errInvalidParameterCombo      = "InvalidParameterCombination"
	instanceStatusOK              = "ok"
	instanceStatusNotApplicable   = "not-applicable"
	instanceStatusReachability    = "reachability"
	instanceStatusDetailPassed    = "passed"
	describeInstanceStatusMinPage = 5
	describeInstanceStatusMaxPage = 1000
)

// DescribeInstanceAttribute handles the DescribeInstanceAttribute action.
// AWS returns exactly one attribute per call, named by the Attribute
// parameter (e.g. "disableApiTermination").
func (s *Service) DescribeInstanceAttribute(w http.ResponseWriter, r *http.Request) {
	if !decodeEC2Form(w, r) {
		return
	}

	instanceID := r.Form.Get("InstanceId")
	if !requireEC2Parameter(w, instanceID == "", "InstanceId") {
		return
	}

	attribute := r.Form.Get("Attribute")
	if !requireEC2Parameter(w, attribute == "", "Attribute") {
		return
	}

	inst, err := s.storage.DescribeInstanceAttribute(r.Context(), instanceID)
	if err != nil {
		handleError(w, err)

		return
	}

	resp, ok := describeInstanceAttributeResponse(inst, attribute)
	if !ok {
		writeError(w, errInvalidParameter, invalidInstanceAttributeMessage(attribute), http.StatusBadRequest)

		return
	}

	writeEC2XMLResponse(w, resp)
}

// ModifyInstanceAttribute handles the ModifyInstanceAttribute action. Both the
// structured parameters (DisableApiTermination.Value, GroupId.N, ...) and the
// legacy Attribute/Value pair are accepted.
func (s *Service) ModifyInstanceAttribute(w http.ResponseWriter, r *http.Request) {
	if !decodeEC2Form(w, r) {
		return
	}

	instanceID := r.Form.Get("InstanceId")
	if !requireEC2Parameter(w, instanceID == "", "InstanceId") {
		return
	}

	updates, err := parseInstanceAttributeUpdates(r.Form)
	if err != nil {
		handleError(w, err)

		return
	}

	if err := s.storage.ModifyInstanceAttribute(r.Context(), instanceID, &updates); err != nil {
		handleError(w, err)

		return
	}

	writeEC2XMLResponse(w, struct {
		XMLName   xml.Name `xml:"ModifyInstanceAttributeResponse"`
		Xmlns     string   `xml:"xmlns,attr"`
		RequestID string   `xml:"requestId"`
		Return    bool     `xml:"return"`
	}{Xmlns: ec2XMLNS, RequestID: uuid.New().String(), Return: true})
}

// DescribeInstanceStatus handles the DescribeInstanceStatus action. kumo has
// no health model, so running instances always report "ok" with passing
// reachability checks and every other state reports "not-applicable".
func (s *Service) DescribeInstanceStatus(w http.ResponseWriter, r *http.Request) {
	if !decodeEC2Form(w, r) {
		return
	}

	instanceIDs := parseIndexedListFromForm(r.Form, "InstanceId")
	includeAll := getFormBoolPtr(r.Form, "IncludeAllInstances")
	filters := parseFiltersFromForm(r.Form)

	if err := validateInstanceStatusFilters(filters); err != nil {
		handleError(w, err)

		return
	}

	maxResults, offset, err := parseInstanceStatusPaging(r.Form, len(instanceIDs) > 0)
	if err != nil {
		handleError(w, err)

		return
	}

	instances, err := s.storage.DescribeInstanceStatus(r.Context(), instanceIDs, includeAll != nil && *includeAll)
	if err != nil {
		handleError(w, err)

		return
	}

	items := make([]XMLInstanceStatusItem, 0, len(instances))

	for _, inst := range instances {
		item := instanceStatusItem(inst)
		if matchInstanceStatusFilters(&item, filters) {
			items = append(items, item)
		}
	}

	page, nextToken := paginateInstanceStatus(items, offset, maxResults)

	writeEC2XMLResponse(w, XMLDescribeInstanceStatusResponse{
		Xmlns:             ec2XMLNS,
		RequestID:         uuid.New().String(),
		InstanceStatusSet: XMLInstanceStatusSet{Items: page},
		NextToken:         nextToken,
	})
}

// instanceAttributeRenderers fills in the response element for each attribute
// name AWS defines for DescribeInstanceAttribute.
var instanceAttributeRenderers = map[string]func(*XMLDescribeInstanceAttributeResponse, *Instance){
	"instanceType": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.InstanceType = &xmlStringValue{Value: i.InstanceType}
	},
	"kernel": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.Kernel = &xmlStringValue{Value: i.KernelID}
	},
	"ramdisk": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.Ramdisk = &xmlStringValue{Value: i.RamdiskID}
	},
	"userData": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.UserData = &xmlStringValue{Value: i.UserData}
	},
	"disableApiTermination": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.DisableAPITermination = &xmlBoolValue{Value: i.DisableAPITermination}
	},
	"disableApiStop": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.DisableAPIStop = &xmlBoolValue{Value: i.DisableAPIStop}
	},
	"instanceInitiatedShutdownBehavior": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.InstanceInitiatedShutdownBehavior = &xmlStringValue{Value: instanceShutdownBehavior(i)}
	},
	"rootDeviceName": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.RootDeviceName = &xmlStringValue{Value: instanceRootDeviceName(i)}
	},
	"blockDeviceMapping": func(r *XMLDescribeInstanceAttributeResponse, _ *Instance) { r.BlockDeviceMapping = &xmlEmptySet{} },
	"productCodes":       func(r *XMLDescribeInstanceAttributeResponse, _ *Instance) { r.ProductCodes = &xmlEmptySet{} },
	"sourceDestCheck": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.SourceDestCheck = &xmlBoolValue{Value: instanceSourceDestCheck(i)}
	},
	"groupSet": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) { r.GroupSet = instanceGroupSet(i) },
	"ebsOptimized": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.EbsOptimized = &xmlBoolValue{Value: i.EbsOptimized}
	},
	"enaSupport": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.EnaSupport = &xmlBoolValue{Value: i.EnaSupport}
	},
	"sriovNetSupport": func(r *XMLDescribeInstanceAttributeResponse, i *Instance) {
		r.SriovNetSupport = &xmlStringValue{Value: i.SriovNetSupport}
	},
	"enclaveOptions": func(r *XMLDescribeInstanceAttributeResponse, _ *Instance) {
		r.EnclaveOptions = &xmlEnclaveOptions{Enabled: false}
	},
}

// describeInstanceAttributeResponse renders the single requested attribute.
// It returns false for an attribute name AWS does not define.
func describeInstanceAttributeResponse(inst *Instance, attribute string) (*XMLDescribeInstanceAttributeResponse, bool) {
	render, ok := instanceAttributeRenderers[attribute]
	if !ok {
		return nil, false
	}

	resp := &XMLDescribeInstanceAttributeResponse{
		Xmlns:      ec2XMLNS,
		RequestID:  uuid.New().String(),
		InstanceID: inst.InstanceID,
	}
	render(resp, inst)

	return resp, true
}

func instanceGroupSet(inst *Instance) *XMLGroupSet {
	items := make([]XMLGroupIdentifier, 0, len(inst.SecurityGroups))
	for _, sg := range inst.SecurityGroups {
		items = append(items, XMLGroupIdentifier(sg))
	}

	return &XMLGroupSet{Items: items}
}

func instanceShutdownBehavior(inst *Instance) string {
	if inst.InstanceInitiatedShutdownBehavior == "" {
		return defaultShutdownBehavior
	}

	return inst.InstanceInitiatedShutdownBehavior
}

func instanceRootDeviceName(inst *Instance) string {
	if inst.RootDeviceName == "" {
		return defaultInstanceRootDeviceName
	}

	return inst.RootDeviceName
}

func instanceAvailabilityZone(inst *Instance) string {
	if inst.AvailabilityZone == "" {
		return defaultInstanceAvailabilityZone
	}

	return inst.AvailabilityZone
}

func instanceSourceDestCheck(inst *Instance) bool {
	return inst.SourceDestCheck == nil || *inst.SourceDestCheck
}

func invalidInstanceAttributeMessage(attribute string) string {
	return fmt.Sprintf("Value (%s) for parameter attribute is invalid. Unknown attribute.", attribute)
}

// instanceAttrParam describes one attribute ModifyInstanceAttribute can
// change: its AWS attribute name (used by the legacy Attribute/Value form),
// its structured request parameter, and how to record a raw value.
type instanceAttrParam struct {
	name  string
	param string
	set   func(u *InstanceAttributeUpdates, value string) error
}

func stringInstanceAttr(name, param string, field func(*InstanceAttributeUpdates) **string) instanceAttrParam {
	return instanceAttrParam{name: name, param: param, set: func(u *InstanceAttributeUpdates, value string) error {
		*field(u) = &value

		return nil
	}}
}

func boolInstanceAttr(name, param string, field func(*InstanceAttributeUpdates) **bool) instanceAttrParam {
	return instanceAttrParam{name: name, param: param, set: func(u *InstanceAttributeUpdates, value string) error {
		b, err := strconv.ParseBool(value)
		if err != nil {
			return &Error{
				Code:    errInvalidParameter,
				Message: fmt.Sprintf("Value (%s) for parameter %s is invalid. Expected a boolean.", value, name),
			}
		}

		*field(u) = &b

		return nil
	}}
}

var modifiableInstanceAttrs = []instanceAttrParam{
	stringInstanceAttr("instanceType", "InstanceType", func(u *InstanceAttributeUpdates) **string { return &u.InstanceType }),
	stringInstanceAttr("kernel", "Kernel", func(u *InstanceAttributeUpdates) **string { return &u.KernelID }),
	stringInstanceAttr("ramdisk", "Ramdisk", func(u *InstanceAttributeUpdates) **string { return &u.RamdiskID }),
	stringInstanceAttr("userData", "UserData", func(u *InstanceAttributeUpdates) **string { return &u.UserData }),
	stringInstanceAttr("instanceInitiatedShutdownBehavior", "InstanceInitiatedShutdownBehavior",
		func(u *InstanceAttributeUpdates) **string { return &u.InstanceInitiatedShutdownBehavior }),
	stringInstanceAttr("sriovNetSupport", "SriovNetSupport", func(u *InstanceAttributeUpdates) **string { return &u.SriovNetSupport }),
	boolInstanceAttr("disableApiTermination", "DisableApiTermination", func(u *InstanceAttributeUpdates) **bool { return &u.DisableAPITermination }),
	boolInstanceAttr("disableApiStop", "DisableApiStop", func(u *InstanceAttributeUpdates) **bool { return &u.DisableAPIStop }),
	boolInstanceAttr("sourceDestCheck", "SourceDestCheck", func(u *InstanceAttributeUpdates) **bool { return &u.SourceDestCheck }),
	boolInstanceAttr("ebsOptimized", "EbsOptimized", func(u *InstanceAttributeUpdates) **bool { return &u.EbsOptimized }),
	boolInstanceAttr("enaSupport", "EnaSupport", func(u *InstanceAttributeUpdates) **bool { return &u.EnaSupport }),
}

// parseInstanceAttributeUpdates reads ModifyInstanceAttribute parameters from
// the form. Structured parameters arrive as "<Param>.Value", security groups
// as "GroupId.N", and the legacy form as "Attribute" plus "Value".
// BlockDeviceMapping.N is accepted but not modeled.
func parseInstanceAttributeUpdates(form map[string][]string) (InstanceAttributeUpdates, error) {
	var u InstanceAttributeUpdates

	for _, attr := range modifiableInstanceAttrs {
		if v, ok := formFirstValue(form, attr.param+".Value"); ok {
			if err := attr.set(&u, v); err != nil {
				return u, err
			}
		}
	}

	if groups := parseIndexedListFromForm(form, "GroupId"); len(groups) > 0 {
		u.GroupIDs = groups
	}

	if attribute, ok := formFirstValue(form, "Attribute"); ok {
		if err := applyLegacyInstanceAttribute(&u, form, attribute); err != nil {
			return u, err
		}
	}

	if b := u.InstanceInitiatedShutdownBehavior; b != nil && *b != "stop" && *b != "terminate" {
		return u, &Error{
			Code:    errInvalidParameter,
			Message: fmt.Sprintf("Value (%s) for parameter instanceInitiatedShutdownBehavior is invalid. Valid values are stop and terminate.", *b),
		}
	}

	return u, nil
}

// applyLegacyInstanceAttribute handles the Attribute/Value request form,
// where Attribute names the attribute and Value carries its new value.
// groupSet takes its value from GroupId.N instead.
func applyLegacyInstanceAttribute(u *InstanceAttributeUpdates, form map[string][]string, attribute string) error {
	if attribute == "groupSet" {
		if u.GroupIDs == nil {
			return &Error{Code: errMissingParameter, Message: "The request must contain the parameter groupId"}
		}

		return nil
	}

	for _, attr := range modifiableInstanceAttrs {
		if attr.name != attribute {
			continue
		}

		v, ok := formFirstValue(form, "Value")
		if !ok {
			return &Error{Code: errMissingParameter, Message: "The request must contain the parameter value"}
		}

		return attr.set(u, v)
	}

	return &Error{Code: errInvalidParameter, Message: invalidInstanceAttributeMessage(attribute)}
}

func formFirstValue(form map[string][]string, key string) (string, bool) {
	values, ok := form[key]
	if !ok || len(values) == 0 {
		return "", false
	}

	return values[0], true
}

// instanceStatusItem derives the status entry reported for an instance.
func instanceStatusItem(inst *Instance) XMLInstanceStatusItem {
	summary := XMLInstanceStatusSummary{Status: instanceStatusNotApplicable}

	if inst.State.Code == InstanceStateRunning {
		summary = XMLInstanceStatusSummary{
			Status: instanceStatusOK,
			Details: XMLInstanceStatusDetailsSet{Items: []XMLInstanceStatusDetail{
				{Name: instanceStatusReachability, Status: instanceStatusDetailPassed},
			}},
		}
	}

	return XMLInstanceStatusItem{
		InstanceID:       inst.InstanceID,
		AvailabilityZone: instanceAvailabilityZone(inst),
		InstanceState:    XMLInstanceState{Code: inst.State.Code, Name: inst.State.Name},
		SystemStatus:     summary,
		InstanceStatus:   summary,
	}
}

// instanceStatusFilterValue extracts the value a DescribeInstanceStatus
// filter compares against.
var instanceStatusFilterValue = map[string]func(*XMLInstanceStatusItem) string{
	"availability-zone":            func(i *XMLInstanceStatusItem) string { return i.AvailabilityZone },
	"instance-state-name":          func(i *XMLInstanceStatusItem) string { return i.InstanceState.Name },
	"instance-state-code":          func(i *XMLInstanceStatusItem) string { return strconv.Itoa(i.InstanceState.Code) },
	"instance-status.status":       func(i *XMLInstanceStatusItem) string { return i.InstanceStatus.Status },
	"system-status.status":         func(i *XMLInstanceStatusItem) string { return i.SystemStatus.Status },
	"instance-status.reachability": func(i *XMLInstanceStatusItem) string { return i.InstanceStatus.reachability() },
	"system-status.reachability":   func(i *XMLInstanceStatusItem) string { return i.SystemStatus.reachability() },
}

func validateInstanceStatusFilters(filters map[string][]string) error {
	for name := range filters {
		if _, ok := instanceStatusFilterValue[name]; !ok {
			return &Error{Code: errInvalidParameter, Message: fmt.Sprintf("The filter '%s' is invalid", name)}
		}
	}

	return nil
}

// matchInstanceStatusFilters reports whether the item satisfies every filter;
// within one filter any listed value matches.
func matchInstanceStatusFilters(item *XMLInstanceStatusItem, filters map[string][]string) bool {
	for name, values := range filters {
		if !containsString(values, instanceStatusFilterValue[name](item)) {
			return false
		}
	}

	return true
}

// parseInstanceStatusPaging reads MaxResults and NextToken. A zero maxResults
// means "no limit". AWS rejects MaxResults combined with explicit instance IDs.
func parseInstanceStatusPaging(form map[string][]string, hasInstanceIDs bool) (maxResults, offset int, err error) {
	if v, ok := formFirstValue(form, "MaxResults"); ok {
		if hasInstanceIDs {
			return 0, 0, &Error{
				Code:    errInvalidParameterCombo,
				Message: "The parameter instancesSet cannot be used with the parameter maxResults",
			}
		}

		maxResults, err = strconv.Atoi(v)
		if err != nil || maxResults < describeInstanceStatusMinPage || maxResults > describeInstanceStatusMaxPage {
			return 0, 0, &Error{
				Code: errInvalidParameter,
				Message: fmt.Sprintf("Value (%s) for parameter maxResults is invalid. Expecting a value between %d and %d.",
					v, describeInstanceStatusMinPage, describeInstanceStatusMaxPage),
			}
		}
	}

	if v, ok := formFirstValue(form, "NextToken"); ok && v != "" {
		offset, err = strconv.Atoi(v)
		if err != nil || offset < 0 {
			return 0, 0, &Error{Code: errInvalidParameter, Message: "Invalid value for parameter nextToken"}
		}
	}

	return maxResults, offset, nil
}

// paginateInstanceStatus returns the page starting at offset and the token
// for the following page, or "" when this is the last page.
func paginateInstanceStatus(items []XMLInstanceStatusItem, offset, maxResults int) ([]XMLInstanceStatusItem, string) {
	if offset >= len(items) {
		return []XMLInstanceStatusItem{}, ""
	}

	items = items[offset:]
	if maxResults == 0 || len(items) <= maxResults {
		return items, ""
	}

	return items[:maxResults], strconv.Itoa(offset + maxResults)
}
