package cbs

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/dbdd4us/qcloudapi-sdk-go/metadata"
	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/util/resizefs"
	"k8s.io/kubernetes/pkg/volume"
	"k8s.io/utils/exec"
	"k8s.io/utils/mount"

	cbs "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/cbs/v20170312"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"

	"github.com/tencentcloud/kubernetes-csi-tencentcloud/driver/util"
)

var (
	DiskByIDDevicePath       = "/dev/disk/by-id"
	DiskByIDDeviceNamePrefix = "virtio-"

	MaxAttachVolumePerNode = 20

	nodeCaps = []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_EXPAND_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	}
)

type cbsNode struct {
	metadataClient *metadata.MetaData
	cbsClient      *cbs.Client
	mounter        mount.SafeFormatAndMount
	idempotent     *util.Idempotent
}

// TODO  node plugin need idempotent and should use inflight
func newCbsNode(secretId, secretKey, region string) (*cbsNode, error) {
	client, err := cbs.NewClient(common.NewCredential(secretId, secretKey), region, profile.NewClientProfile())
	if err != nil {
		return nil, err
	}

	node := cbsNode{
		metadataClient: metadata.NewMetaData(http.DefaultClient),
		cbsClient:      client,
		mounter: mount.SafeFormatAndMount{
			Interface: mount.New(""),
			Exec:      exec.New(),
		},
		idempotent: util.NewIdempotent(),
	}
	return &node, nil
}

func (node *cbsNode) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	glog.Infof("NodeStageVolume: start with args %v", *req)

	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is empty")
	}
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume staging target path is empty")
	}
	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume has no capabilities")
	}
	// cbs is not support rawblock currently
	if req.VolumeCapability.GetMount() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume access type is not mount")
	}

	// 1. check if current req is in progress.
	if ok := node.idempotent.Insert(req); !ok {
		msg := fmt.Sprintf("volume %v is in progress", req.VolumeId)
		return nil, status.Error(codes.Internal, msg)
	}

	defer func() {
		glog.Infof("NodeStageVolume: volume %v finished", req.VolumeId)
		node.idempotent.Delete(req)
	}()

	diskID := req.VolumeId

	stagingTargetPath := req.StagingTargetPath

	mountFlags := req.VolumeCapability.GetMount().MountFlags
	mountFsType := req.VolumeCapability.GetMount().FsType

	if _, err := os.Stat(stagingTargetPath); err != nil {
		if os.IsNotExist(err) {
			err := os.MkdirAll(stagingTargetPath, 0750)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	//2. check target path mounted
	cbsDisk := filepath.Join(DiskByIDDevicePath, DiskByIDDeviceNamePrefix+diskID)
	diskSource, err := findCBSVolume(cbsDisk)
	if err != nil {
		glog.Infof("NodeStageVolume: findCBSVolume error cbs disk=%v, error %v", cbsDisk, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	device, _, err := mount.GetDeviceNameFromMount(node.mounter, stagingTargetPath)
	if err != nil {
		glog.Errorf("NodeStageVolume: GetDeviceNameFromMount error %v", err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if diskSource == device {
		glog.Infof("NodeStageVolume: volume %v already staged", diskID)
		return &csi.NodeStageVolumeResponse{}, nil
	}

	if err := node.mounter.FormatAndMount(diskSource, stagingTargetPath, mountFsType, mountFlags); err != nil {
		glog.Errorf(
			"NodeStageVolume: FormatAndMount error diskSource %v stagingTargetPath %v, error %v",
			diskSource, stagingTargetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (node *cbsNode) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	glog.Infof("NodeUnstageVolume: start with args %v", *req)

	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume staging target path is empty")
	}
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is empty")
	}

	stagingTargetPath := req.StagingTargetPath

	_, refCount, err := mount.GetDeviceNameFromMount(node.mounter, stagingTargetPath)
	fmt.Printf("refCount is %v", refCount)
	if err != nil {
		glog.Errorf("NodeUnstageVolume: GetDeviceNameFromMount error %v", err)
		return nil, status.Error(codes.Internal, err.Error())
	}
	if refCount == 0 {
		glog.Infof("NodeUnstageVolume: %v is not mounted", stagingTargetPath)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	if err := node.mounter.Unmount(stagingTargetPath); err != nil {
		glog.Errorf("NodeUnstageVolume: Unmount %v error %v", stagingTargetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (node *cbsNode) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.VolumeId == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is empty")
	}
	if req.StagingTargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume staging target path is empty")
	}
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume target path is empty")
	}
	if req.VolumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume has no capabilities")
	}

	if req.VolumeCapability.GetMount() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume access type is not mount")
	}

	source := req.StagingTargetPath
	target := req.TargetPath

	mountFlags := req.VolumeCapability.GetMount().MountFlags
	mountFlags = append(mountFlags, "bind")

	if req.Readonly {
		mountFlags = append(mountFlags, "ro")
	}

	mountFsType := req.VolumeCapability.GetMount().FsType

	if mountFsType == "" {
		mountFsType = "ext4"
	}

	if _, err := os.Stat(target); err != nil {
		if os.IsNotExist(err) {
			err := os.MkdirAll(target, 0750)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if err := node.mounter.Mount(source, target, mountFsType, mountFlags); err != nil {
		glog.Errorf("NodePublishVolume: Mount error target %v error %v", target, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (node *cbsNode) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.TargetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume target path is empty")
	}

	targetPath := req.TargetPath

	if err := node.mounter.Unmount(targetPath); err != nil {
		glog.Errorf("NodeUnpublishVolume: Mount error targetPath %v error %v", targetPath, err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (node *cbsNode) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	glog.Infof("NodeGetCapabilities: called with args %+v", *req)
	var caps []*csi.NodeServiceCapability
	for _, cap := range nodeCaps {
		c := &csi.NodeServiceCapability{
			Type: &csi.NodeServiceCapability_Rpc{
				Rpc: &csi.NodeServiceCapability_RPC{
					Type: cap,
				},
			},
		}
		caps = append(caps, c)
	}
	return &csi.NodeGetCapabilitiesResponse{Capabilities: caps}, nil
}

func (node *cbsNode) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	nodeID, err := node.metadataClient.InstanceID()
	if err != nil {
		glog.Errorf("NodeGetInfo node.metadataClient.InstanceID() error: %v", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	zone, err := node.metadataClient.Zone()
	if err != nil {
		glog.Errorf("NodeGetInfo node.metadataClient.Zone() error: %v", err)
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeGetInfoResponse{
		NodeId:            nodeID,
		MaxVolumesPerNode: int64(MaxAttachVolumePerNode),

		// make sure that the driver works on this particular zone only
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				TopologyZoneKey: zone,
			},
		},
	}, nil
}

func (node *cbsNode) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	glog.Infof("NodeGetVolumeStats: NodeGetVolumeStatsRequest is %v", *req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}
	VolumePath := req.GetVolumePath()
	if len(VolumePath) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume path is empty")
	}

	volumeMetrics, err := volume.NewMetricsStatFS(req.VolumePath).GetMetrics()
	if err != nil {
		return nil, err
	}

	available, ok := volumeMetrics.Available.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "Volume metrics available %v is invalid", volumeMetrics.Available)
	}
	capacity, ok := volumeMetrics.Capacity.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "Volume metrics capacity %v is invalid", volumeMetrics.Capacity)
	}
	used, ok := volumeMetrics.Used.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "Volume metrics used %v is invalid", volumeMetrics.Used)
	}

	inodesFree, ok := volumeMetrics.InodesFree.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "Volume metrics inodesFree %v is invalid", volumeMetrics.InodesFree)
	}
	inodes, ok := volumeMetrics.Inodes.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "Volume metrics inodes %v is invalid", volumeMetrics.Inodes)
	}
	inodesUsed, ok := volumeMetrics.InodesUsed.AsInt64()
	if !ok {
		return nil, status.Errorf(codes.Internal, "Volume metrics inodesUsed %v is invalid", volumeMetrics.InodesUsed)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: available,
				Total:     capacity,
				Used:      used,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: inodesFree,
				Total:     inodes,
				Used:      inodesUsed,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}

func (node *cbsNode) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	glog.Infof("NodeExpandVolume: NodeExpandVolumeRequest is %v", *req)

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID not provided")
	}

	args := []string{"-o", "source", "--noheadings", "--target", req.GetVolumePath()}
	output, err := node.mounter.Exec.Command("findmnt", args...).Output()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "Could not determine device path: %v, raw block device or unmounted", err)
	}

	devicePath := strings.TrimSpace(string(output))
	if len(devicePath) == 0 {
		return nil, status.Errorf(codes.Internal, "Could not get valid device for mount path: %v", req.GetVolumePath())
	}
	r := resizefs.NewResizeFs(&node.mounter)
	if _, err := r.Resize(devicePath, req.GetVolumePath()); err != nil {
		return nil, status.Errorf(codes.Internal, "Could not resize volume %s %s:  %v", volumeID, devicePath, err)
	}

	return &csi.NodeExpandVolumeResponse{}, nil
}

func findCBSVolume(p string) (device string, err error) {
	stat, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			glog.Infof("cbs block path %q not found", p)
			return "", fmt.Errorf("cbs block path %q not found", p)
		}
		return "", fmt.Errorf("error getting stat of %q: %v", p, err)
	}

	if stat.Mode()&os.ModeSymlink != os.ModeSymlink {
		glog.Warningf("cbs block file %q found, but was not a symlink", p)
		return "", fmt.Errorf("cbs block file %q found, but was not a symlink", p)
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("error reading target of symlink %q: %v", p, err)
	}

	if !strings.HasPrefix(resolved, "/dev") {
		return "", fmt.Errorf("resolved symlink for %q was unexpected: %q", p, resolved)
	}

	return resolved, nil
}
