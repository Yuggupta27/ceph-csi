package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	. "github.com/onsi/ginkgo" // nolint
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/kubernetes/test/e2e/framework"
	e2elog "k8s.io/kubernetes/test/e2e/framework/log"
)

var _ = Describe("RBD Upgrade Testing", func() {
	f := framework.NewDefaultFramework("upgrade-test-rbd")
	var (
		// cwd stores the initial working directory.
		cwd string
		c   clientset.Interface
		pvc *v1.PersistentVolumeClaim
		app *v1.Pod
		// checkSum stores the md5sum of a file to verify uniqueness.
		checkSum string
	)

	// deploy rbd CSI
	BeforeEach(func() {
		if !upgradeTesting || !testRBD {
			Skip("Skipping RBD Upgrade Testing")
		}
		c = f.ClientSet
		if cephCSINamespace != defaultNs {
			err := createNamespace(c, cephCSINamespace)
			if err != nil {
				Fail(err.Error())
			}
		}
		createNodeLabel(f, nodeRegionLabel, regionValue)
		createNodeLabel(f, nodeZoneLabel, zoneValue)

		// fetch current working directory to switch back
		// when we are done upgrading.
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			Fail(err.Error())
		}

		deployVault(f.ClientSet, deployTimeout)
		err = upgradeAndDeployCSI(upgradeVersion, "rbd")
		if err != nil {
			Fail(err.Error())
		}
		createConfigMap(rbdDirPath, f.ClientSet, f)
		createRBDStorageClass(f.ClientSet, f, nil, nil)
		createRBDSecret(f.ClientSet, f)
	})
	AfterEach(func() {
		if !testRBD || !upgradeTesting {
			Skip("Skipping RBD Upgrade Testing")
		}
		if CurrentGinkgoTestDescription().Failed {
			// log pods created by helm chart
			logsCSIPods("app=ceph-csi-rbd", c)
			// log provisoner
			logsCSIPods("app=csi-rbdplugin-provisioner", c)
			// log node plugin
			logsCSIPods("app=csi-rbdplugin", c)
		}

		deleteConfigMap(rbdDirPath)
		deleteResource(rbdExamplePath + "secret.yaml")
		deleteResource(rbdExamplePath + "storageclass.yaml")
		deleteVault()
		if deployRBD {
			deleteRBDPlugin()
			if cephCSINamespace != defaultNs {
				err := deleteNamespace(c, cephCSINamespace)
				if err != nil {
					Fail(err.Error())
				}
			}
		}
		deleteNodeLabel(c, nodeRegionLabel)
		deleteNodeLabel(c, nodeZoneLabel)
	})

	Context("Test RBD CSI", func() {
		It("Test RBD CSI", func() {
			pvcPath := rbdExamplePath + "pvc.yaml"
			appPath := rbdExamplePath + "pod.yaml"

			By("checking provisioner deployment is running", func() {
				err := waitForDeploymentComplete(rbdDeploymentName, cephCSINamespace, f.ClientSet, deployTimeout)
				if err != nil {
					Fail(err.Error())
				}
			})

			By("checking nodeplugin deamonsets is running", func() {
				err := waitForDaemonSets(rbdDaemonsetName, cephCSINamespace, f.ClientSet, deployTimeout)
				if err != nil {
					Fail(err.Error())
				}
			})

			By("upgrade to latest changes and verify app re-mount", func() {
				// TODO: fetch pvc size from spec.
				pvcSize := "2Gi"
				var err error
				pvc, err = loadPVC(pvcPath)
				if pvc == nil {
					Fail(err.Error())
				}
				pvc.Namespace = f.UniqueName
				e2elog.Logf("The PVC  template %+v", pvc)

				app, err = loadApp(appPath)
				if err != nil {
					Fail(err.Error())
				}
				app.Namespace = f.UniqueName
				app.Labels = map[string]string{"app": "upgrade-testing"}
				pvc.Spec.Resources.Requests[v1.ResourceStorage] = resource.MustParse(pvcSize)
				err = createPVCAndApp("", f, pvc, app, deployTimeout)
				if err != nil {
					Fail(err.Error())
				}
				opt := metav1.ListOptions{
					LabelSelector: "app=upgrade-testing",
				}
				// fetch the path where volume is mounted.
				mountPath := getMountPath(app)
				filePath := filepath.Join(mountPath, "testClone")

				// create a test file at the mountPath.
				cmd := fmt.Sprintf("touch %s", filePath)
				_, stdErr := execCommandInPod(f, cmd, app.Namespace, &opt)
				if stdErr != "" {
					e2elog.Logf("failed to create file %s", stdErr)
				}

				opt = metav1.ListOptions{
					LabelSelector: "app=upgrade-testing",
				}
				e2elog.Logf("Calculating checksum of %s", filePath)
				checkSum, err = calculateMd5Sum(f, app, filePath, &opt)
				if err != nil {
					Fail(err.Error())
				}

				// Create snapshot of the pvc
				snapshotPath := rbdExamplePath + "snapshot.yaml"
				createRBDSnapshotClass(f)
				snap := getSnapshot(snapshotPath)
				snap.Name = "rbd-pvc-snapshot"
				snap.Namespace = f.UniqueName
				snap.Spec.Source.PersistentVolumeClaimName = &pvc.Name
				// var s v1beta1.VolumeSnapshot
				err = createSnapshot(&snap, deployTimeout)
				if err != nil {
					e2elog.Logf("failed to create snapshot %v", err)
					Fail(err.Error())
				}

				err = deletePod(app.Name, app.Namespace, f.ClientSet, deployTimeout)
				if err != nil {
					Fail(err.Error())
				}
				deleteRBDPlugin()

				err = os.Chdir(cwd)
				if err != nil {
					Fail(err.Error())
				}

				deployRBDPlugin()
				// validate if the app gets bound to a pvc created by
				// an earlier release.
				app.Labels = map[string]string{"app": "upgrade-testing"}
				err = createApp(f.ClientSet, app, deployTimeout)
				if err != nil {
					Fail(err.Error())
				}
			})

			By("Create clone from a snapshot", func() {
				pvcSize := "2Gi"
				pvcClonePath := rbdExamplePath + "pvc-restore.yaml"
				// pvcSmartClonePath := rbdExamplePath + "pvc-clone.yaml"
				appClonePath := rbdExamplePath + "pod-restore.yaml"
				v, err := f.ClientSet.Discovery().ServerVersion()
				if err != nil {
					e2elog.Logf("failed to get server version with error %v", err)
					Fail(err.Error())
				}
				// pvc clone is only supported from v1.16+
				if v.Major > "1" || (v.Major == "1" && v.Minor >= "16") {
					pvcClone, err := loadPVC(pvcClonePath)
					if err != nil {
						Fail(err.Error())
					}
					pvcClone.Namespace = f.UniqueName
					pvcClone.Spec.Resources.Requests[v1.ResourceStorage] = resource.MustParse(pvcSize)
					pvcClone.Spec.DataSource.Name = "rbd-pvc-snapshot"
					appClone, err := loadApp(appClonePath)
					if err != nil {
						Fail(err.Error())
					}
					appClone.Namespace = f.UniqueName
					appClone.Name = "app-clone-from-snap"
					appClone.Labels = map[string]string{"app": "validate-snap-clone"}
					err = createPVCAndApp("", f, pvcClone, appClone, deployTimeout)
					if err != nil {
						Fail(err.Error())
					}
					opt := metav1.ListOptions{
						LabelSelector: "app=validate-snap-clone",
					}
					e2elog.Logf("Calculating mountPath")
					mountPath := getMountPath(appClone)
					e2elog.Logf("Calculating testFilePath")
					testFilePath := filepath.Join(mountPath, "testClone")
					e2elog.Logf("Calculating newCheckSum")
					newCheckSum, err := calculateMd5Sum(f, appClone, testFilePath, &opt)
					if err != nil {
						Fail(err.Error())
					}

					if !strings.Contains(newCheckSum, checkSum) {
						e2elog.Logf("The md5sum of files did not match, expected %s received %s  ", checkSum, newCheckSum)
						Fail(err.Error())
					}
					e2elog.Logf("The md5sum of files matched")

					// delete cloned pvc and pod
					err = deletePVCAndApp("", f, pvcClone, appClone)
					if err != nil {
						Fail(err.Error())
					}

				}
			})

			By("Create clone from existing PVC", func() {
				pvcSize := "2Gi"
				pvcSmartClonePath := rbdExamplePath + "pvc-clone.yaml"
				appSmartClonePath := rbdExamplePath + "pod-clone.yaml"
				v, err := f.ClientSet.Discovery().ServerVersion()
				if err != nil {
					e2elog.Logf("failed to get server version with error %v", err)
					Fail(err.Error())
				}
				// pvc clone is only supported from v1.16+
				if v.Major > "1" || (v.Major == "1" && v.Minor >= "16") {
					pvcClone, err := loadPVC(pvcSmartClonePath)
					if err != nil {
						Fail(err.Error())
					}
					pvcClone.Spec.DataSource.Name = pvc.Name
					pvcClone.Namespace = f.UniqueName
					pvcClone.Spec.Resources.Requests[v1.ResourceStorage] = resource.MustParse(pvcSize)
					appClone, err := loadApp(appSmartClonePath)
					if err != nil {
						Fail(err.Error())
					}
					appClone.Namespace = f.UniqueName
					appClone.Name = "appclone"
					appClone.Labels = map[string]string{"app": "validate-clone"}
					err = createPVCAndApp("", f, pvcClone, appClone, deployTimeout)
					if err != nil {
						Fail(err.Error())
					}
					opt := metav1.ListOptions{
						LabelSelector: "app=validate-clone",
					}
					mountPath := getMountPath(appClone)
					testFilePath := filepath.Join(mountPath, "testClone")
					newCheckSum, err := calculateMd5Sum(f, appClone, testFilePath, &opt)
					if err != nil {
						Fail(err.Error())
					}

					if !strings.Contains(newCheckSum, checkSum) {
						e2elog.Logf("The md5sum of files did not match, expected %s received %s  ", checkSum, newCheckSum)
						Fail(err.Error())
					}
					e2elog.Logf("The md5sum of files matched")

					// delete cloned pvc and pod
					err = deletePVCAndApp("", f, pvcClone, appClone)
					if err != nil {
						Fail(err.Error())
					}

				}
			})

			By("Resize pvc and verify expansion", func() {
				pvcExpandSize := "5Gi"

				v, err := f.ClientSet.Discovery().ServerVersion()
				if err != nil {
					e2elog.Logf("failed to get server version with error %v", err)
					Fail(err.Error())
				}
				// Resize 0.3.0 is only supported from v1.15+
				if v.Major > "1" || (v.Major == "1" && v.Minor >= "15") {
					opt := metav1.ListOptions{
						LabelSelector: "app=upgrade-testing",
					}
					pvc, err = f.ClientSet.CoreV1().PersistentVolumeClaims(pvc.Namespace).Get(context.TODO(), pvc.Name, metav1.GetOptions{})
					if err != nil {
						Fail(err.Error())
					}

					// resize PVC
					err = expandPVCSize(f.ClientSet, pvc, pvcExpandSize, deployTimeout)
					if err != nil {
						Fail(err.Error())
					}
					// wait for application pod to come up after resize
					err = waitForPodInRunningState(app.Name, app.Namespace, f.ClientSet, deployTimeout)
					if err != nil {
						Fail(err.Error())
					}
					// validate if resize is successful.
					err = checkDirSize(app, f, &opt, pvcExpandSize)
					if err != nil {
						Fail(err.Error())
					}
				}

			})

			By("delete pvc and app", func() {
				err := deletePVCAndApp("", f, pvc, app)
				if err != nil {
					Fail(err.Error())
				}
			})
		})
	})
})
