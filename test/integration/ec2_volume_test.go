//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"github.com/sivchari/golden"
)

var volumeIgnoreFields = golden.WithIgnoreFields("VolumeId", "InstanceId", "CreateTime", "AttachTime", "ResultMetadata")

func runTestInstance(t *testing.T, client *ec2.Client) string {
	t.Helper()

	result, err := client.RunInstances(t.Context(), &ec2.RunInstancesInput{
		ImageId:      aws.String("ami-12345678"),
		InstanceType: types.InstanceTypeT2Micro,
		MinCount:     aws.Int32(1),
		MaxCount:     aws.Int32(1),
	})
	if err != nil {
		t.Fatalf("failed to run instance: %v", err)
	}

	instanceID := *result.Instances[0].InstanceId

	t.Cleanup(func() {
		_, _ = client.TerminateInstances(context.Background(), &ec2.TerminateInstancesInput{
			InstanceIds: []string{instanceID},
		})
	})

	return instanceID
}

func createTestVolume(t *testing.T, client *ec2.Client, input *ec2.CreateVolumeInput) *ec2.CreateVolumeOutput {
	t.Helper()

	result, err := client.CreateVolume(t.Context(), input)
	if err != nil {
		t.Fatalf("failed to create volume: %v", err)
	}

	volumeID := *result.VolumeId

	t.Cleanup(func() {
		_, _ = client.DetachVolume(context.Background(), &ec2.DetachVolumeInput{VolumeId: aws.String(volumeID)})
		_, _ = client.DeleteVolume(context.Background(), &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)})
	})

	return result
}

func assertEC2ErrorCode(t *testing.T, err error, want string) {
	t.Helper()

	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != want {
		t.Fatalf("expected %s, got: %v", want, err)
	}
}

func TestEC2_VolumeAttachDetach(t *testing.T) {
	client := newEC2Client(t)
	ctx := t.Context()

	instanceID := runTestInstance(t, client)

	createResult := createTestVolume(t, client, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(20),
		VolumeType:       types.VolumeTypeGp3,
		TagSpecifications: []types.TagSpecification{{
			ResourceType: types.ResourceTypeVolume,
			Tags:         []types.Tag{{Key: aws.String("Name"), Value: aws.String("data")}},
		}},
	})
	golden.New(t, volumeIgnoreFields).Assert(t.Name()+"_create", createResult)

	volumeID := *createResult.VolumeId

	attachResult, err := client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(instanceID),
		Device:     aws.String("/dev/sdf"),
	})
	if err != nil {
		t.Fatalf("failed to attach volume: %v", err)
	}
	golden.New(t, volumeIgnoreFields).Assert(t.Name()+"_attach", attachResult)

	if got := aws.ToString(attachResult.VolumeId); got != volumeID {
		t.Errorf("attach VolumeId = %q, want %q", got, volumeID)
	}

	if got := aws.ToString(attachResult.InstanceId); got != instanceID {
		t.Errorf("attach InstanceId = %q, want %q", got, instanceID)
	}

	// The filter shape terraform's aws_volume_attachment uses on read.
	descResult, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []types.Filter{
			{Name: aws.String("attachment.instance-id"), Values: []string{instanceID}},
			{Name: aws.String("attachment.device"), Values: []string{"/dev/sdf"}},
			{Name: aws.String("volume-id"), Values: []string{volumeID}},
		},
	})
	if err != nil {
		t.Fatalf("failed to describe volumes: %v", err)
	}
	golden.New(t, volumeIgnoreFields).Assert(t.Name()+"_describe_attached", descResult)

	_, err = client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)})
	assertEC2ErrorCode(t, err, "VolumeInUse")

	detachResult, err := client.DetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId:   aws.String(volumeID),
		InstanceId: aws.String(instanceID),
	})
	if err != nil {
		t.Fatalf("failed to detach volume: %v", err)
	}
	golden.New(t, volumeIgnoreFields).Assert(t.Name()+"_detach", detachResult)

	// Once detached, the attachment filters must exclude the volume; terraform
	// relies on this to see that aws_volume_attachment is gone.
	filtered, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{
		Filters: []types.Filter{
			{Name: aws.String("attachment.instance-id"), Values: []string{instanceID}},
			{Name: aws.String("attachment.device"), Values: []string{"/dev/sdf"}},
			{Name: aws.String("volume-id"), Values: []string{volumeID}},
		},
	})
	if err != nil {
		t.Fatalf("failed to describe volumes: %v", err)
	}

	if len(filtered.Volumes) != 0 {
		t.Errorf("filtered DescribeVolumes after detach returned %d volumes, want 0", len(filtered.Volumes))
	}

	_, err = client.DetachVolume(ctx, &ec2.DetachVolumeInput{VolumeId: aws.String(volumeID)})
	assertEC2ErrorCode(t, err, "IncorrectState")

	descResult, err = client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volumeID}})
	if err != nil {
		t.Fatalf("failed to describe volumes: %v", err)
	}
	golden.New(t, volumeIgnoreFields).Assert(t.Name()+"_describe_detached", descResult)

	if _, err := client.DeleteVolume(ctx, &ec2.DeleteVolumeInput{VolumeId: aws.String(volumeID)}); err != nil {
		t.Fatalf("failed to delete volume: %v", err)
	}

	_, err = client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{volumeID}})
	assertEC2ErrorCode(t, err, "InvalidVolume.NotFound")
}

func TestEC2_AttachVolume_Errors(t *testing.T) {
	client := newEC2Client(t)
	ctx := t.Context()

	instanceID := runTestInstance(t, client)

	first := createTestVolume(t, client, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(8),
	})
	second := createTestVolume(t, client, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(8),
	})

	if _, err := client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   first.VolumeId,
		InstanceId: aws.String(instanceID),
		Device:     aws.String("/dev/sdf"),
	}); err != nil {
		t.Fatalf("failed to attach volume: %v", err)
	}

	otherInstanceID := runTestInstance(t, client)

	_, err := client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   first.VolumeId,
		InstanceId: aws.String(otherInstanceID),
		Device:     aws.String("/dev/sdg"),
	})
	assertEC2ErrorCode(t, err, "VolumeInUse")

	_, err = client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   second.VolumeId,
		InstanceId: aws.String(instanceID),
		Device:     aws.String("/dev/sdf"),
	})
	assertEC2ErrorCode(t, err, "InvalidParameterValue")

	_, err = client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   aws.String("vol-doesnotexist"),
		InstanceId: aws.String(instanceID),
		Device:     aws.String("/dev/sdh"),
	})
	assertEC2ErrorCode(t, err, "InvalidVolume.NotFound")

	_, err = client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   second.VolumeId,
		InstanceId: aws.String("i-doesnotexist"),
		Device:     aws.String("/dev/sdh"),
	})
	assertEC2ErrorCode(t, err, "InvalidInstanceID.NotFound")

	_, err = client.DetachVolume(ctx, &ec2.DetachVolumeInput{
		VolumeId:   first.VolumeId,
		InstanceId: aws.String(otherInstanceID),
	})
	assertEC2ErrorCode(t, err, "InvalidAttachment.NotFound")
}

func TestEC2_TerminateInstances_ReleasesVolumes(t *testing.T) {
	client := newEC2Client(t)
	ctx := t.Context()

	instanceID := runTestInstance(t, client)

	vol := createTestVolume(t, client, &ec2.CreateVolumeInput{
		AvailabilityZone: aws.String("us-east-1a"),
		Size:             aws.Int32(8),
	})

	if _, err := client.AttachVolume(ctx, &ec2.AttachVolumeInput{
		VolumeId:   vol.VolumeId,
		InstanceId: aws.String(instanceID),
		Device:     aws.String("/dev/sdf"),
	}); err != nil {
		t.Fatalf("failed to attach volume: %v", err)
	}

	if _, err := client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{
		InstanceIds: []string{instanceID},
	}); err != nil {
		t.Fatalf("failed to terminate instance: %v", err)
	}

	descResult, err := client.DescribeVolumes(ctx, &ec2.DescribeVolumesInput{VolumeIds: []string{*vol.VolumeId}})
	if err != nil {
		t.Fatalf("failed to describe volumes: %v", err)
	}

	got := descResult.Volumes[0]
	if got.State != types.VolumeStateAvailable || len(got.Attachments) != 0 {
		t.Errorf("volume after terminate: state=%s attachments=%d, want available with none", got.State, len(got.Attachments))
	}
}
