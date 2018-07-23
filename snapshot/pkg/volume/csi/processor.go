/*
Copyright 2018 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package csi

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"time"

	yaml "gopkg.in/yaml.v1"
	"k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/golang/glog"

	crdv1 "github.com/kubernetes-incubator/external-storage/snapshot/pkg/apis/crd/v1"
	"github.com/kubernetes-incubator/external-storage/snapshot/pkg/cloudprovider"
	"github.com/kubernetes-incubator/external-storage/snapshot/pkg/volume"
	k8sVol "k8s.io/kubernetes/pkg/volume/util"

	csipb "github.com/container-storage-interface/spec/lib/go/csi/v0"
	"google.golang.org/grpc"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	csiPathTemplate               = "/var/lib/kubelet/plugins/%v/"
	addrFile                      = "csi.sock"
	parametersFile                = "parameters.yaml"
	csiTimeout                    = 15 * time.Second
	provisionerSecretNameKey      = "csiProvisionerSecretName"
	provisionerSecretNamespaceKey = "csiProvisionerSecretNamespace"
)

type csiPlugin struct {
	driverName string
	secrets    map[string]string
	parameters map[string]string
}

var _ volume.Plugin = &csiPlugin{}

// Init inits volume plugin
func (c *csiPlugin) Init(_ cloudprovider.Interface) {
}

func buildConfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	return rest.InClusterConfig()
}

func getSecretsFromParameters(parameters map[string]string, kubeconfig string) map[string]string {
	if len(parameters[provisionerSecretNameKey]) == 0 ||
		len(parameters[provisionerSecretNamespaceKey]) == 0 {
		return nil
	}

	secretName := parameters[provisionerSecretNameKey]
	secretNamespace := parameters[provisionerSecretNamespaceKey]

	config, err := buildConfig(kubeconfig)
	if err != nil {
		glog.Errorf("Can't access kubernetes with config. kubeconfig is required to use CSI plugin.")
		return nil
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		glog.Errorf("Failed to retrieve secret %s in %s from the API server: %q", secretName, secretNamespace, err)
		return nil
	}

	secret, err := clientset.CoreV1().Secrets(secretNamespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		glog.Errorf("Failed to retrieve secret %s in %s from the API server: %q", secretName, secretNamespace, err)
		return nil
	}

	secrets := map[string]string{}
	for key, value := range secret.Data {
		secrets[key] = string(value)
	}

	return secrets
}

type conf struct {
	Parameters map[string]string `yaml:"parameters,omitempty"`
}

func getParametersFromFile(driverName string) map[string]string {
	parametersPath := fmt.Sprintf(csiPathTemplate+"parameters/"+parametersFile, driverName)

	parameters, err := ioutil.ReadFile(parametersPath)
	if err != nil {
		glog.Errorf("Failed to read parameter %s", parametersPath)
		return nil
	}

	cfg := conf{}
	err = yaml.Unmarshal(parameters, &cfg)
	if err != nil {
		glog.Errorf("Failed to unmarsahl parameter %s", parametersPath)
		return nil
	}

	return cfg.Parameters
}

// RegisterPlugin creates an uninitialized csi plugin
func RegisterPlugin(name string, kubeconfig string) volume.Plugin {
	parameters := getParametersFromFile(name)

	return &csiPlugin{
		driverName: name,
		secrets:    getSecretsFromParameters(parameters, kubeconfig),
		parameters: parameters,
	}
}

// GetPluginName retrieves the name of the plugin
func GetPluginName() string {
	return "csi"
}

func newGrpcConn(driverName string) (*grpc.ClientConn, error) {
	if driverName == "" {
		return nil, fmt.Errorf("driver name is empty")
	}
	addr := fmt.Sprintf(csiPathTemplate+addrFile, driverName)

	network := "unix"
	glog.V(4).Infof("creating new gRPC connection for [%s://%s]", network, addr)

	return grpc.Dial(
		addr,
		grpc.WithInsecure(),
		grpc.WithDialer(func(target string, timeout time.Duration) (net.Conn, error) {
			return net.Dial(network, target)
		}),
	)
}

// VolumeDelete deletes the specified volume pased on pv
func (c *csiPlugin) VolumeDelete(pv *v1.PersistentVolume) error {
	if pv == nil || pv.Spec.CSI == nil {
		return fmt.Errorf("invalid CSI PV: %v", pv)
	}
	if c.driverName != pv.Spec.CSI.Driver {
		return fmt.Errorf("Can't support CSI driver: %v", pv.Spec.CSI.Driver)
	}
	volumeID := pv.Spec.CSI.VolumeHandle

	conn, err := newGrpcConn(c.driverName)
	if err != nil {
		return err
	}
	defer conn.Close()
	nodeControllerClient := csipb.NewControllerClient(conn)

	req := &csipb.DeleteVolumeRequest{
		VolumeId:                volumeID,
		ControllerDeleteSecrets: c.secrets,
	}

	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()
	_, err = nodeControllerClient.DeleteVolume(ctx, req)
	if err != nil {
		return err
	}

	return nil
}

// SnapshotCreate creates a VolumeSnapshot from a PersistentVolumeSpec
func (c *csiPlugin) SnapshotCreate(
	_ *crdv1.VolumeSnapshot,
	pv *v1.PersistentVolume,
	tags *map[string]string,
) (*crdv1.VolumeSnapshotDataSource, *[]crdv1.VolumeSnapshotCondition, error) {
	spec := &pv.Spec
	if spec == nil || spec.CSI == nil {
		return nil, nil, fmt.Errorf("invalid PV spec %v", spec)
	}
	if c.driverName != pv.Spec.CSI.Driver {
		return nil, nil, fmt.Errorf("Can't support CSI driver: %v", pv.Spec.CSI.Driver)
	}
	volumeID := spec.CSI.VolumeHandle
	snapshotName := string(pv.Name) + fmt.Sprintf("%d", time.Now().UnixNano())

	conn, err := newGrpcConn(c.driverName)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	nodeControllerClient := csipb.NewControllerClient(conn)

	req := &csipb.CreateSnapshotRequest{
		SourceVolumeId: volumeID,
		Name:           snapshotName,
		CreateSnapshotSecrets: c.secrets,
		Parameters:            c.parameters,
	}

	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()
	snapshotResponse, err := nodeControllerClient.CreateSnapshot(ctx, req)
	if err != nil {
		return nil, nil, err
	}

	return &crdv1.VolumeSnapshotDataSource{
		CSISnapshot: &crdv1.CSIVolumeSnapshotSource{
			SnapshotID: snapshotResponse.Snapshot.Id,
			DriverName: c.driverName,
		},
	}, c.convertSnapshotStatus(snapshotResponse.Snapshot.Status), nil
}

// SnapshotDelete deletes a VolumeSnapshot
// PersistentVolume is provided for volume types, if any, that need PV Spec to delete snapshot
func (c *csiPlugin) SnapshotDelete(src *crdv1.VolumeSnapshotDataSource, pv *v1.PersistentVolume) error {
	if src == nil || src.CSISnapshot == nil {
		return fmt.Errorf("invalid VolumeSnapshotDataSource: %v", src)
	}

	if pv != nil && c.driverName != pv.Spec.CSI.Driver {
		return fmt.Errorf("Can't support CSI driver: %v", pv.Spec.CSI.Driver)
	}
	snapshotID := src.CSISnapshot.SnapshotID

	conn, err := newGrpcConn(c.driverName)
	if err != nil {
		return err
	}
	defer conn.Close()
	nodeControllerClient := csipb.NewControllerClient(conn)

	req := &csipb.DeleteSnapshotRequest{
		SnapshotId:            snapshotID,
		DeleteSnapshotSecrets: c.secrets,
	}

	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()
	_, err = nodeControllerClient.DeleteSnapshot(ctx, req)
	if err != nil {
		return err
	}

	return nil
}

func asCSIAccessModes(ams []v1.PersistentVolumeAccessMode) csipb.VolumeCapability_AccessMode_Mode {
	if ams == nil {
		return csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	}

	// only use first access mode
	switch ams[0] {
	case v1.ReadWriteOnce:
		return csipb.VolumeCapability_AccessMode_SINGLE_NODE_WRITER
	case v1.ReadOnlyMany:
		return csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY
	case v1.ReadWriteMany:
		return csipb.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER
	}
	return csipb.VolumeCapability_AccessMode_UNKNOWN
}

func isReadOnly(am csipb.VolumeCapability_AccessMode_Mode) bool {
	if am == csipb.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY ||
		am == csipb.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY {
		return true
	} else {
		return false
	}
}

// SnapshotRestore creates a new Volume using the data on the specified Snapshot
func (c *csiPlugin) SnapshotRestore(snapshotData *crdv1.VolumeSnapshotData, pvc *v1.PersistentVolumeClaim, pvName string, parameters map[string]string) (*v1.PersistentVolumeSource, map[string]string, error) {
	if snapshotData == nil || snapshotData.Spec.CSISnapshot == nil {
		return nil, nil, fmt.Errorf("failed to retrieve Snapshot spec")
	}
	if pvc == nil {
		return nil, nil, fmt.Errorf("no pvc specified")
	}
	if c.driverName != snapshotData.Spec.CSISnapshot.DriverName {
		return nil, nil, fmt.Errorf("Can't support CSI driver: %v", snapshotData.Spec.CSISnapshot.DriverName)
	}
	snapID := snapshotData.Spec.CSISnapshot.SnapshotID
	capacity := pvc.Spec.Resources.Requests[v1.ResourceName(v1.ResourceStorage)]
	requestedSz := capacity.Value()
	szGB := k8sVol.RoundUpSize(requestedSz, 1024*1024*1024)

	volCapability := csipb.VolumeCapability{}

	fsType := "ext4"
	if _, ok := parameters["fsType"]; ok {
		fsType = parameters["fsType"]
	}

	volumeMode := pvc.Spec.VolumeMode
	if volumeMode != nil && *volumeMode == v1.PersistentVolumeBlock {
		volCapability.AccessType = &csipb.VolumeCapability_Block{}
	} else {
		volCapability.AccessType = &csipb.VolumeCapability_Mount{
			Mount: &csipb.VolumeCapability_MountVolume{
				FsType: fsType,
			},
		}
	}

	volCapability.AccessMode = &csipb.VolumeCapability_AccessMode{
		Mode: asCSIAccessModes(pvc.Spec.AccessModes),
	}

	conn, err := newGrpcConn(c.driverName)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	nodeControllerClient := csipb.NewControllerClient(conn)

	req := &csipb.CreateVolumeRequest{
		Name: pvName,
		CapacityRange: &csipb.CapacityRange{
			RequiredBytes: int64(szGB),
		},
		VolumeCapabilities: []*csipb.VolumeCapability{
			&volCapability,
		},
		Parameters:              parameters,
		ControllerCreateSecrets: c.secrets,
		VolumeContentSource: &csipb.VolumeContentSource{
			Type: &csipb.VolumeContentSource_Snapshot{
				Snapshot: &csipb.VolumeContentSource_SnapshotSource{
					Id: snapID,
				},
			},
		},
		// TODO: support topology aware volume.
		//AccessibilityRequirements: nil,
	}

	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()
	volume, err := nodeControllerClient.CreateVolume(ctx, req)
	if err != nil {
		glog.Errorf("error create volume from snapshot: %v", err)
		return nil, nil, err
	}

	glog.V(2).Infof("Successfully created CSI Volume from Snapshot, Volume: %s", volume.Volume.Id)
	pv := &v1.PersistentVolumeSource{
		CSI: &v1.CSIPersistentVolumeSource{
			Driver:           c.driverName,
			VolumeHandle:     volume.Volume.Id,
			FSType:           fsType,
			VolumeAttributes: parameters,
			ReadOnly:         isReadOnly(volCapability.AccessMode.Mode),
			NodePublishSecretRef: &v1.SecretReference{
				Name:      parameters["csiProvisionerSecretName"],
				Namespace: parameters["csiProvisionerSecretNamespace"],
			},
		},
	}
	return pv, nil, nil
}

// DescribeSnapshot retrieves info for the specified Snapshot
func (c *csiPlugin) DescribeSnapshot(snapshotData *crdv1.VolumeSnapshotData) (*[]crdv1.VolumeSnapshotCondition, bool, error) {
	if snapshotData == nil || snapshotData.Spec.CSISnapshot == nil {
		return nil, false, fmt.Errorf("invalid VolumeSnapshotDataSource: %v", snapshotData)
	}
	if c.driverName != snapshotData.Spec.CSISnapshot.DriverName {
		return nil, false, fmt.Errorf("Can't support CSI driver: %v", snapshotData.Spec.CSISnapshot.DriverName)
	}
	snapshotID := snapshotData.Spec.CSISnapshot.SnapshotID

	conn, err := newGrpcConn(c.driverName)
	if err != nil {
		return nil, false, err
	}
	defer conn.Close()
	nodeControllerClient := csipb.NewControllerClient(conn)

	req := &csipb.ListSnapshotsRequest{
		SnapshotId: snapshotID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), csiTimeout)
	defer cancel()
	snapshots, err := nodeControllerClient.ListSnapshots(ctx, req)
	if err != nil {
		return c.convertSnapshotStatus(snapshots.Entries[0].Snapshot.Status), false, err
	}

	isComplete := (snapshots.Entries[0].Snapshot.Status.GetType().String() == "READY")
	glog.Infof("DescribeSnapshot: Snapshot %s, Status %s, isComplete: %v", snapshotID, snapshots.Entries[0].Snapshot.Status.GetType(), isComplete)

	return c.convertSnapshotStatus(snapshots.Entries[0].Snapshot.Status), isComplete, nil
}

// FindSnapshot finds a VolumeSnapshot by matching metadata
func (c *csiPlugin) FindSnapshot(tags *map[string]string) (*crdv1.VolumeSnapshotDataSource, *[]crdv1.VolumeSnapshotCondition, error) {
	glog.Infof("FindSnapshot by tags: %#v", *tags)

	// TODO: Implement FindSnapshot
	return nil, nil, fmt.Errorf("Snapshot not found")
}

// convertSnapshotStatus converts CSI snapshot status to crdv1.VolumeSnapshotCondition
func (c *csiPlugin) convertSnapshotStatus(status *csipb.SnapshotStatus) *[]crdv1.VolumeSnapshotCondition {
	var snapConditions []crdv1.VolumeSnapshotCondition
	if status.GetType().String() == "READY" {
		snapConditions = []crdv1.VolumeSnapshotCondition{
			{
				Type:               crdv1.VolumeSnapshotConditionReady,
				Status:             v1.ConditionTrue,
				Message:            "Snapshot created successfully and it is ready",
				LastTransitionTime: metav1.Now(),
			},
		}
	} else if status.GetType().String() == "UPLOADING" {
		snapConditions = []crdv1.VolumeSnapshotCondition{
			{
				Type:               crdv1.VolumeSnapshotConditionPending,
				Status:             v1.ConditionUnknown,
				Message:            "Snapshot is being created",
				LastTransitionTime: metav1.Now(),
			},
		}
	} else {
		snapConditions = []crdv1.VolumeSnapshotCondition{
			{
				Type:               crdv1.VolumeSnapshotConditionError,
				Status:             v1.ConditionTrue,
				Message:            "Snapshot creation failed",
				LastTransitionTime: metav1.Now(),
			},
		}
	}

	return &snapConditions
}
