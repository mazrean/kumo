package ec2

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Literals shared across the instance attribute tests.
const (
	formKeyInstanceID            = "InstanceId"
	formKeyAttribute             = "Attribute"
	formKeyAction                = "Action"
	formValueTrue                = "true"
	testInstanceTypeLarge        = "t3.large"
	testGroupA                   = "sg-a"
	testImageID                  = "ami-1"
	actionDescribeInstanceStatus = "DescribeInstanceStatus"
)

func runTestInstance(t *testing.T, s *MemoryStorage, req *RunInstancesRequest) *Instance {
	t.Helper()

	instances, _, err := s.RunInstances(t.Context(), req)
	if err != nil {
		t.Fatalf("RunInstances: %v", err)
	}

	return instances[0]
}

func wantEC2ErrorCode(t *testing.T, err error, code string) {
	t.Helper()

	var ec2Err *Error
	if !errors.As(err, &ec2Err) || ec2Err.Code != code {
		t.Fatalf("err = %v, want code %s", err, code)
	}
}

func TestParseInstanceAttributeUpdates_Structured(t *testing.T) {
	form := map[string][]string{
		formKeyInstanceID:                 {"i-1"},
		"DisableApiTermination.Value":     {formValueTrue},
		"SourceDestCheck.Value":           {"false"},
		"InstanceType.Value":              {testInstanceTypeLarge},
		"GroupId.2":                       {"sg-b"},
		"GroupId.1":                       {testGroupA},
		"BlockDeviceMapping.1.DeviceName": {"/dev/xvda"},
	}

	u, err := parseInstanceAttributeUpdates(form)
	if err != nil {
		t.Fatal(err)
	}

	if u.DisableAPITermination == nil || !*u.DisableAPITermination {
		t.Errorf("DisableAPITermination = %v, want true", u.DisableAPITermination)
	}

	if u.SourceDestCheck == nil || *u.SourceDestCheck {
		t.Errorf("SourceDestCheck = %v, want false", u.SourceDestCheck)
	}

	if u.InstanceType == nil || *u.InstanceType != testInstanceTypeLarge {
		t.Errorf("InstanceType = %v, want t3.large", u.InstanceType)
	}

	if got := strings.Join(u.GroupIDs, ","); got != "sg-a,sg-b" {
		t.Errorf("GroupIDs = %q, want sg-a,sg-b", got)
	}

	if u.DisableAPIStop != nil || u.UserData != nil {
		t.Errorf("unset attributes must stay nil: %+v", u)
	}
}

func TestParseInstanceAttributeUpdates_Legacy(t *testing.T) {
	u, err := parseInstanceAttributeUpdates(map[string][]string{
		formKeyAttribute: {"instanceInitiatedShutdownBehavior"},
		"Value":          {"terminate"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if u.InstanceInitiatedShutdownBehavior == nil || *u.InstanceInitiatedShutdownBehavior != "terminate" {
		t.Errorf("InstanceInitiatedShutdownBehavior = %v, want terminate", u.InstanceInitiatedShutdownBehavior)
	}

	u, err = parseInstanceAttributeUpdates(map[string][]string{
		formKeyAttribute: {"groupSet"},
		"GroupId.1":      {testGroupA},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(u.GroupIDs) != 1 || u.GroupIDs[0] != testGroupA {
		t.Errorf("GroupIDs = %v, want [sg-a]", u.GroupIDs)
	}
}

func TestParseInstanceAttributeUpdates_Errors(t *testing.T) {
	tests := []struct {
		name string
		form map[string][]string
		code string
	}{
		{"bad bool", map[string][]string{"EbsOptimized.Value": {"maybe"}}, errInvalidParameter},
		{"bad shutdown behavior", map[string][]string{"InstanceInitiatedShutdownBehavior.Value": {"hibernate"}}, errInvalidParameter},
		{"unknown legacy attribute", map[string][]string{formKeyAttribute: {"rootDeviceName"}, "Value": {"/dev/sda1"}}, errInvalidParameter},
		{"legacy without value", map[string][]string{formKeyAttribute: {"disableApiStop"}}, errMissingParameter},
		{"legacy groupSet without groups", map[string][]string{formKeyAttribute: {"groupSet"}}, errMissingParameter},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseInstanceAttributeUpdates(tt.form)
			wantEC2ErrorCode(t, err, tt.code)
		})
	}
}

func TestModifyInstanceAttribute_RequiresStoppedForInstanceType(t *testing.T) {
	s := NewMemoryStorage()
	inst := runTestInstance(t, s, &RunInstancesRequest{ImageID: testImageID, InstanceType: "t2.micro"})
	newType := testInstanceTypeLarge

	err := s.ModifyInstanceAttribute(t.Context(), inst.InstanceID, &InstanceAttributeUpdates{InstanceType: &newType})
	wantEC2ErrorCode(t, err, errCodeIncorrectInstanceState)

	if _, err := s.StopInstances(t.Context(), []string{inst.InstanceID}); err != nil {
		t.Fatal(err)
	}

	if err := s.ModifyInstanceAttribute(t.Context(), inst.InstanceID, &InstanceAttributeUpdates{InstanceType: &newType}); err != nil {
		t.Fatal(err)
	}

	got, err := s.DescribeInstanceAttribute(t.Context(), inst.InstanceID)
	if err != nil {
		t.Fatal(err)
	}

	if got.InstanceType != newType {
		t.Errorf("InstanceType = %q, want %q", got.InstanceType, newType)
	}
}

func TestModifyInstanceAttribute_UnknownGroupLeavesInstanceUntouched(t *testing.T) {
	s := NewMemoryStorage()
	inst := runTestInstance(t, s, &RunInstancesRequest{ImageID: testImageID})
	protect := true

	err := s.ModifyInstanceAttribute(t.Context(), inst.InstanceID, &InstanceAttributeUpdates{
		DisableAPITermination: &protect,
		GroupIDs:              []string{"sg-missing"},
	})
	wantEC2ErrorCode(t, err, errCodeInvalidGroupNotFound)

	got, _ := s.DescribeInstanceAttribute(t.Context(), inst.InstanceID)
	if got.DisableAPITermination {
		t.Error("DisableAPITermination applied despite failed update")
	}
}

func TestTerminationAndStopProtection(t *testing.T) {
	s := NewMemoryStorage()
	protected := runTestInstance(t, s, &RunInstancesRequest{ImageID: testImageID, DisableAPITermination: true, DisableAPIStop: true})
	other := runTestInstance(t, s, &RunInstancesRequest{ImageID: testImageID})

	_, err := s.TerminateInstances(t.Context(), []string{other.InstanceID, protected.InstanceID})
	wantEC2ErrorCode(t, err, errCodeOperationNotPermitted)

	_, err = s.StopInstances(t.Context(), []string{protected.InstanceID})
	wantEC2ErrorCode(t, err, errCodeOperationNotPermitted)

	got, _ := s.DescribeInstanceAttribute(t.Context(), other.InstanceID)
	if got.State.Code != InstanceStateRunning {
		t.Errorf("unprotected instance in a rejected batch changed state to %s", got.State.Name)
	}

	off := false
	if err := s.ModifyInstanceAttribute(t.Context(), protected.InstanceID, &InstanceAttributeUpdates{DisableAPITermination: &off}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.TerminateInstances(t.Context(), []string{protected.InstanceID}); err != nil {
		t.Errorf("terminate after lifting protection: %v", err)
	}
}

func TestDescribeInstanceStatus_Storage(t *testing.T) {
	s := NewMemoryStorage()
	running := runTestInstance(t, s, &RunInstancesRequest{ImageID: testImageID})
	stopped := runTestInstance(t, s, &RunInstancesRequest{ImageID: testImageID})

	if _, err := s.StopInstances(t.Context(), []string{stopped.InstanceID}); err != nil {
		t.Fatal(err)
	}

	got, err := s.DescribeInstanceStatus(t.Context(), nil, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].InstanceID != running.InstanceID {
		t.Errorf("default = %d instances, want only the running one", len(got))
	}

	got, err = s.DescribeInstanceStatus(t.Context(), []string{stopped.InstanceID}, true)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].InstanceID != stopped.InstanceID {
		t.Errorf("includeAll by ID = %v, want the stopped instance", got)
	}

	_, err = s.DescribeInstanceStatus(t.Context(), []string{"i-missing"}, false)
	wantEC2ErrorCode(t, err, errCodeInvalidInstanceIDNotFound)
}

func TestPaginateInstanceStatus(t *testing.T) {
	items := make([]XMLInstanceStatusItem, 7)

	page, next := paginateInstanceStatus(items, 0, 5)
	if len(page) != 5 || next != "5" {
		t.Fatalf("first page = %d items, token %q; want 5, \"5\"", len(page), next)
	}

	page, next = paginateInstanceStatus(items, 5, 5)
	if len(page) != 2 || next != "" {
		t.Fatalf("second page = %d items, token %q; want 2, \"\"", len(page), next)
	}
}

func dispatchEC2(t *testing.T, svc *Service, form url.Values) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/?Action="+form.Get(formKeyAction), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	svc.DispatchAction(rec, req)

	return rec
}

func TestInstanceAttributeHandlers(t *testing.T) {
	store := NewMemoryStorage()
	svc := New(store)
	inst := runTestInstance(t, store, &RunInstancesRequest{ImageID: testImageID, UserData: "ZWNobyBoaQ=="})

	rec := dispatchEC2(t, svc, url.Values{
		formKeyAction:                 {"ModifyInstanceAttribute"},
		formKeyInstanceID:             {inst.InstanceID},
		"DisableApiTermination.Value": {formValueTrue},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("Modify status = %d: %s", rec.Code, rec.Body)
	}

	tests := []struct {
		attribute string
		want      string
	}{
		{"disableApiTermination", "<disableApiTermination><value>true</value></disableApiTermination>"},
		{"userData", "<userData><value>ZWNobyBoaQ==</value></userData>"},
		{"kernel", "<kernel></kernel>"},
		{"instanceInitiatedShutdownBehavior", "<value>stop</value>"},
		{"sourceDestCheck", "<sourceDestCheck><value>true</value></sourceDestCheck>"},
		{"rootDeviceName", "<value>/dev/xvda</value>"},
	}

	for _, tt := range tests {
		t.Run(tt.attribute, func(t *testing.T) {
			rec := dispatchEC2(t, svc, url.Values{
				formKeyAction:     {"DescribeInstanceAttribute"},
				formKeyInstanceID: {inst.InstanceID},
				formKeyAttribute:  {tt.attribute},
			})
			if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tt.want) {
				t.Errorf("status %d, body %s; want %s", rec.Code, rec.Body, tt.want)
			}
		})
	}

	rec = dispatchEC2(t, svc, url.Values{
		formKeyAction:     {"DescribeInstanceAttribute"},
		formKeyInstanceID: {inst.InstanceID},
		formKeyAttribute:  {"bogus"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown attribute status = %d, want 400", rec.Code)
	}
}

func TestDescribeInstanceStatusHandler(t *testing.T) {
	store := NewMemoryStorage()
	svc := New(store)
	running := runTestInstance(t, store, &RunInstancesRequest{ImageID: testImageID})
	stopped := runTestInstance(t, store, &RunInstancesRequest{ImageID: testImageID})

	if _, err := store.StopInstances(t.Context(), []string{stopped.InstanceID}); err != nil {
		t.Fatal(err)
	}

	rec := dispatchEC2(t, svc, url.Values{
		formKeyAction:         {actionDescribeInstanceStatus},
		"IncludeAllInstances": {formValueTrue},
		"Filter.1.Name":       {"instance-status.status"},
		"Filter.1.Value.1":    {"not-applicable"},
	})
	body := rec.Body.String()

	if rec.Code != http.StatusOK || !strings.Contains(body, stopped.InstanceID) || strings.Contains(body, running.InstanceID) {
		t.Errorf("filtered status = %d, body %s; want only %s", rec.Code, body, stopped.InstanceID)
	}

	rec = dispatchEC2(t, svc, url.Values{formKeyAction: {actionDescribeInstanceStatus}})
	body = rec.Body.String()

	if !strings.Contains(body, "<status>ok</status>") || !strings.Contains(body, "<name>reachability</name>") {
		t.Errorf("running instance status body = %s", body)
	}

	rec = dispatchEC2(t, svc, url.Values{
		formKeyAction:   {actionDescribeInstanceStatus},
		"Filter.1.Name": {"bogus"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown filter status = %d, want 400", rec.Code)
	}

	rec = dispatchEC2(t, svc, url.Values{
		formKeyAction:  {actionDescribeInstanceStatus},
		"InstanceId.1": {running.InstanceID},
		"MaxResults":   {"5"},
	})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), errInvalidParameterCombo) {
		t.Errorf("MaxResults with InstanceId = %d %s, want InvalidParameterCombination", rec.Code, rec.Body)
	}
}
