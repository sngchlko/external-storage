## Quick Howto

#### Prepare a Ceph-CSI PV
We use external ceph-csi provisioner

[ceph-csi example](https://github.com/ceph/ceph-csi/tree/master/deploy/rbd/kubernetes)

* Create Secret
    ```bash
    kubectl create -f https://raw.githubusercontent.com/ceph/ceph-csi/master/deploy/rbd/kubernetes/rbd-secrets.yaml
    ```
    Important: rbd-secrets.yaml, must be customized to match your ceph environment.

* Create StorageClass
    ```bash
    kubectl create -f ./deploy/rbd/kubernetes/rbd-storage-class.yaml
    ```
    Important: rbd-storage-class.yaml, must be customized to match your ceph environment.

* Start CSI CEPH RBD plugin
    ```bash
    kubectl create -f https://raw.githubusercontent.com/ceph/ceph-csi/master/deploy/rbd/kubernetes/rbdplugin.yaml
    ```

* Start CSI External Attacher
    ```bash
    kubectl create -f https://raw.githubusercontent.com/ceph/ceph-csi/master/deploy/rbd/kubernetes/csi-attacher.yaml
    ```

* Start CSI External Provisioner
 Must use node labels with csi-plugin!!!!!!!!!!!!!!!
    ```bash
    kubectl create -f https://raw.githubusercontent.com/ceph/ceph-csi/master/deploy/rbd/kubernetes/csi-provisioner.yaml
    ```

* Create a PVC
    ```bash
    kubectl create -f examples/csi/ceph-csi/pvc.yaml
    ```

#### Start Snapshot Controller and PV Provisioner

* Create a ConfigMap
    ```bash
    kubectl create -f examples/csi/ceph-csi/config.map
    ```
    Important: configmap.yaml, must be customized to match your ceph environment.

* Create a RBAC
    ```bash
    kubectl create -f examples/csi/ceph-csi/snapshot-rbac.yaml
    ```

* Create Snapshot Controller and PV Provisioner
    ```bash
    kubectl create -f examples/csi/ceph-csi/deployment.yaml


####  Create a snapshot
Now we have PVC bound to a PV that contains some data. We want to take snapshot of this data so we can restore the data later.

 * Create a Snapshot Third Party Resource
    ```bash
    kubectl create -f examples/csi/ceph-csi/snapshot.yaml
    ```

#### Check VolumeSnapshot and VolumeSnapshotData are created

* Appropriate Kubernetes objects are available and describe the snapshot (output is trimmed for readability):
    ```bash
    kubectl get volumesnapshot,volumesnapshotdata -o yaml
    ```

## Snapshot based PV Provisioner

Unlike exiting PV provisioners that provision blank volume, Snapshot based PV provisioners create volumes based on existing snapshots. Thus new provisioners are needed.

There is a special annotation give to PVCs that request snapshot based PVs. As illustrated in [the example](examples/csi/ceph-csi/claim.yaml), `snapshot.alpha.kubernetes.io` must point to an existing VolumeSnapshot Object
```yaml
metadata:
  name:
  namespace:
  annotations:
    snapshot.alpha.kubernetes.io/snapshot: snapshot-demo
```

## Ceph CSI Volume Type

### Create Storage Class to restore a snapshot to a PV

* Create a storage class:
    ```bash
    kubectl create -f examples/csi/ceph-csi/class.yaml
    ```

### Restore a snapshot to a new PV

* Create a PVC that claims a PV based on an existing snapshot
    ```bash
    kubectl create -f examples/csi/ceph-csi/claim.yaml
    ```
* Check that a PV was created

    ```bash
    kubectl get pv,pvc
    ```
