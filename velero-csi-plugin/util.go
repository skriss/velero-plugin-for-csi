package main

import (
	"fmt"
	"strings"
	"time"

	snapshotv1beta1api "github.com/kubernetes-csi/external-snapshotter/v2/pkg/apis/volumesnapshot/v1beta1"
	snapshotter "github.com/kubernetes-csi/external-snapshotter/v2/pkg/client/clientset/versioned/typed/volumesnapshot/v1beta1"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

const (
	//TODO: use annotation from velero https://github.com/vmware-tanzu/velero/pull/2283
	resticPodAnnotation = "backup.velero.io/backup-volumes"
)

func getPVForPVC(pvc *corev1api.PersistentVolumeClaim, corev1 corev1client.PersistentVolumesGetter) (*corev1api.PersistentVolume, error) {
	if pvc.Spec.VolumeName == "" {
		return nil, errors.Errorf("PVC %s/%s has no volume backing this claim", pvc.Namespace, pvc.Name)
	}
	if pvc.Status.Phase != corev1api.ClaimBound {
		// TODO: confirm if this PVC should be snapshotted if it has no PV bound
		return nil, errors.Errorf("PVC %s/%s is in phase %v and is not bound to a volume", pvc.Namespace, pvc.Name, pvc.Status.Phase)
	}
	pvName := pvc.Spec.VolumeName
	pv, err := corev1.PersistentVolumes().Get(pvName, metav1.GetOptions{})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get PV %s for PVC %s/%s", pvName, pvc.Namespace, pvc.Name)
	}
	return pv, nil
}

func getPodsUsingPVC(pvcNamespace, pvcName string, corev1 corev1client.PodsGetter) ([]corev1api.Pod, error) {
	podsUsingPVC := []corev1api.Pod{}
	podList, err := corev1.Pods(pvcNamespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	for _, p := range podList.Items {
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvcName {
				podsUsingPVC = append(podsUsingPVC, p)
			}
		}
	}

	return podsUsingPVC, nil
}

func getPodVolumeNameForPVC(pod corev1api.Pod, pvcName string) (string, error) {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == pvcName {
			return v.Name, nil
		}
	}
	return "", errors.Errorf("Pod %s/%s does not use PVC %s/%s", pod.Namespace, pod.Name, pod.Namespace, pvcName)
}

func getPodVolumesUsingRestic(pod corev1api.Pod) []string {
	resticAnnotation := pod.Annotations[resticPodAnnotation]
	if resticAnnotation == "" {
		return []string{}
	}
	return strings.Split(pod.Annotations[resticPodAnnotation], ",")
}

func contains(slice []string, key string) bool {
	for _, i := range slice {
		if i == key {
			return true
		}
	}
	return false
}

func isPVCBackedUpByRestic(pvcNamespace, pvcName string, podClient corev1client.PodsGetter) (bool, error) {
	pods, err := getPodsUsingPVC(pvcNamespace, pvcName, podClient)
	if err != nil {
		return false, errors.WithStack(err)
	}

	for _, p := range pods {
		resticVols := getPodVolumesUsingRestic(p)
		if len(resticVols) > 0 {
			volName, err := getPodVolumeNameForPVC(p, pvcName)
			if err != nil {
				return false, err
			}
			if contains(resticVols, volName) {
				return true, nil
			}
		}
	}

	return false, nil
}

func getVolumeSnapshotClassForStorageClass(provisioner string, snapshotClient snapshotter.SnapshotV1beta1Interface) (*snapshotv1beta1api.VolumeSnapshotClass, error) {
	snapshotClasses, err := snapshotClient.VolumeSnapshotClasses().List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "error listing volumesnapshot classes")
	}
	for _, sc := range snapshotClasses.Items {
		if sc.Driver == provisioner {
			return &sc, nil
		}
	}
	return nil, errors.Errorf("failed to get volumesnapshotclass for provisioner %s", provisioner)
}

func getVolumeSnapshotContentForVolumeSnapshot(volSnap *snapshotv1beta1api.VolumeSnapshot, snapshotClient snapshotter.SnapshotV1beta1Interface, log logrus.FieldLogger) (*snapshotv1beta1api.VolumeSnapshotContent, error) {
	var snapshotContent *snapshotv1beta1api.VolumeSnapshotContent
	for {
		vs, err := snapshotClient.VolumeSnapshots(volSnap.Namespace).Get(volSnap.Name, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, fmt.Sprintf("failed to get volumesnapshot %s/%s", volSnap.Namespace, volSnap.Name))
		}

		// TODO: add timeout
		if vs.Status == nil || vs.Status.BoundVolumeSnapshotContentName == nil {
			log.Infof("Waiting for CSI driver to reconcile volumesnapshot %s/%s. Retrying in 5s", volSnap.Namespace, volSnap.Name)
			time.Sleep(5 * time.Second)
			continue
		}
		snapshotContent, err = snapshotClient.VolumeSnapshotContents().Get(*vs.Status.BoundVolumeSnapshotContentName, metav1.GetOptions{})
		if err != nil {
			return nil, errors.Wrapf(err, fmt.Sprintf("failed to get volumesnapshotcontent %s for volumesnapshot %s/%s", *vs.Status.BoundVolumeSnapshotContentName, volSnap.Namespace, volSnap.Name))
		}

		break
	}

	return snapshotContent, nil
}
