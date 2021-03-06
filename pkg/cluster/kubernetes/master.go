package kubernetes

import (
	"encoding/base64"
	"fmt"
	kubekeyapi "github.com/kubesphere/kubekey/pkg/apis/kubekey/v1alpha1"
	"github.com/kubesphere/kubekey/pkg/cluster/kubernetes/tmpl"
	"github.com/kubesphere/kubekey/pkg/plugins/dns"
	"github.com/kubesphere/kubekey/pkg/util/manager"
	"github.com/kubesphere/kubekey/pkg/util/ssh"
	"github.com/pkg/errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	//joinMasterCmd = ""
	//joinWorkerCmd = ""
	//clusterInfo   = ""
	//kubeConfig    = ""
	clusterIsExist = false
	clusterStatus  = map[string]string{
		"version":       "",
		"joinMasterCmd": "",
		"joinWorkerCmd": "",
		"clusterInfo":   "",
		"kubeConfig":    "",
	}
)

func GetClusterStatus(mgr *manager.Manager) error {
	mgr.Logger.Infoln("Get cluster status")

	return mgr.RunTaskOnMasterNodes(getClusterStatus, false)
}

func getClusterStatus(mgr *manager.Manager, node *kubekeyapi.HostCfg, conn ssh.Connection) error {
	if clusterStatus["clusterInfo"] == "" {
		output, err := mgr.Runner.RunCmd("ls /etc/kubernetes/admin.conf")
		if strings.Contains(output, "No such file or directory") {
			clusterIsExist = false
		} else {
			if err != nil {
				return errors.Wrap(errors.WithStack(err), "failed to find /etc/kubernetes/admin.conf")
			} else {
				clusterIsExist = true
				output, err := mgr.Runner.RunCmd("sudo cat /etc/kubernetes/manifests/kube-apiserver.yaml | grep 'image:' | awk -F '[:]' '{print $(NF-0)}'")
				if err != nil {
					return errors.Wrap(errors.WithStack(err), "failed to find current version")
				} else {
					if !strings.Contains(output, "No such file or directory") {
						clusterStatus["version"] = output
					}
				}
				if err := getJoinNodesCmd(mgr); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func InitKubernetesCluster(mgr *manager.Manager) error {
	mgr.Logger.Infoln("Init kubernetes cluster")

	return mgr.RunTaskOnMasterNodes(initKubernetesCluster, true)
}

func initKubernetesCluster(mgr *manager.Manager, node *kubekeyapi.HostCfg, conn ssh.Connection) error {
	if mgr.Runner.Index == 0 && !clusterIsExist {
		kubeadmCfg, err := tmpl.GenerateKubeadmCfg(mgr)
		if err != nil {
			return err
		}
		kubeadmCfgBase64 := base64.StdEncoding.EncodeToString([]byte(kubeadmCfg))
		_, err1 := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"mkdir -p /etc/kubernetes && echo %s | base64 -d > /etc/kubernetes/kubeadm-config.yaml\"", kubeadmCfgBase64))
		if err1 != nil {
			return errors.Wrap(errors.WithStack(err1), "failed to generate kubeadm config")
		}

		output, err2 := mgr.Runner.RunCmd("sudo -E /bin/sh -c \"/usr/local/bin/kubeadm init --config=/etc/kubernetes/kubeadm-config.yaml\"")
		if err2 != nil {
			fmt.Println(output)
			return errors.Wrap(errors.WithStack(err2), "failed to init kubernetes cluster")
		}
		if err3 := GetKubeConfig(mgr); err3 != nil {
			return err3
		}
		if err := removeMasterTaint(mgr, node); err != nil {
			return err
		}
		if err := addWorkerLabel(mgr, node); err != nil {
			return err
		}
		if err := dns.OverrideCorednsService(mgr); err != nil {
			return err
		}
		if err := dns.DeployNodelocaldns(mgr); err != nil {
			return err
		}
		clusterIsExist = true
		if err := getJoinNodesCmd(mgr); err != nil {
			return err
		}
	}

	return nil
}

func GetKubeConfig(mgr *manager.Manager) error {
	createConfigDirCmd := "mkdir -p /root/.kube && mkdir -p $HOME/.kube"
	getKubeConfigCmd := "cp -f /etc/kubernetes/admin.conf /root/.kube/config"
	getKubeConfigCmdUsr := "cp -f /etc/kubernetes/admin.conf $HOME/.kube/config"
	chownKubeConfig := "chown $(id -u):$(id -g) $HOME/.kube/config"

	cmd := strings.Join([]string{createConfigDirCmd, getKubeConfigCmd, getKubeConfigCmdUsr, chownKubeConfig}, " && ")
	_, err := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"%s\"", cmd))
	if err != nil {
		return errors.Wrap(errors.WithStack(err), "failed to init kubernetes cluster")
	}
	return nil
}

func removeMasterTaint(mgr *manager.Manager, node *kubekeyapi.HostCfg) error {
	if node.IsWorker {
		removeMasterTaintCmd := fmt.Sprintf("/usr/local/bin/kubectl taint nodes %s node-role.kubernetes.io/master=:NoSchedule-", node.Name)
		_, err := mgr.Runner.RunCmd(removeMasterTaintCmd)
		if err != nil {
			return errors.Wrap(errors.WithStack(err), "failed to remove master taint")
		}
	}
	return nil
}

func addWorkerLabel(mgr *manager.Manager, node *kubekeyapi.HostCfg) error {
	if node.IsWorker {
		addWorkerLabelCmd := fmt.Sprintf("/usr/local/bin/kubectl label node %s node-role.kubernetes.io/worker=", node.Name)
		out, err := mgr.Runner.RunCmd(addWorkerLabelCmd)
		if err != nil && !strings.Contains(out, "already") {
			return errors.Wrap(errors.WithStack(err), "failed to add worker label")
		}
	}
	return nil
}

//func GetJoinNodesCmd(mgr *manager.Manager) error {
//	mgr.Logger.Infoln("Get join nodes cmd")
//
//	return mgr.RunTaskOnClientNode(getJoinNodesCmd, false)
//}

func getJoinNodesCmd(mgr *manager.Manager) error {
	if err := getJoinCmd(mgr); err != nil {
		return err
	}
	return nil
}

func getJoinCmd(mgr *manager.Manager) error {
	uploadCertsCmd := "/usr/local/bin/kubeadm init phase upload-certs --upload-certs"
	out, err := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"%s\"", uploadCertsCmd))
	if err != nil {
		return errors.Wrap(errors.WithStack(err), "failed to upload kubeadm certs")
	}
	reg := regexp.MustCompile("[0-9|a-z]{64}")
	certificateKey := reg.FindAllString(out, -1)[0]
	err1 := PatchKubeadmSecret(mgr)
	if err1 != nil {
		return err1
	}

	tokenCreateMasterCmd := fmt.Sprintf("/usr/local/bin/kubeadm token create --print-join-command --certificate-key %s", certificateKey)

	out, err2 := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"%s\"", tokenCreateMasterCmd))
	if err2 != nil {
		return errors.Wrap(errors.WithStack(err2), "failed to get join node cmd")
	}

	joinMasterStrList := strings.Split(out, "kubeadm join")
	joinMasterStr := strings.Split(joinMasterStrList[1], certificateKey)
	clusterStatus["joinMasterCmd"] = fmt.Sprintf("/usr/local/bin/kubeadm join %s %s", joinMasterStr[0], certificateKey)

	joinWorkerStrList := strings.Split(clusterStatus["joinMasterCmd"], "--control-plane")
	clusterStatus["joinWorkerCmd"] = joinWorkerStrList[0]

	out, err3 := mgr.Runner.RunCmd("/usr/local/bin/kubectl get nodes -o wide")
	if err3 != nil {
		return errors.Wrap(errors.WithStack(err3), "failed to get cluster info")
	}
	clusterStatus["clusterInfo"] = out

	kubeCfgBase64Cmd := "cat /etc/kubernetes/admin.conf | base64 --wrap=0"
	out, err4 := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"%s\"", kubeCfgBase64Cmd))
	if err4 != nil {
		return errors.Wrap(errors.WithStack(err4), "failed to get cluster kubeconfig")
	}
	clusterStatus["kubeConfig"] = out

	currentDir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		return errors.Wrap(err, "faild get current dir")
	}
	exec.Command(fmt.Sprintf("mkdir -p %s/kubekey", currentDir))
	exec.Command(fmt.Sprintf("sudo -E /bin/sh -c \"echo %s | base64 -d > %s/kubekey/kubeconfig.yaml\"", clusterStatus["kubeConfig"], currentDir)).Run()

	return nil
}

func PatchKubeadmSecret(mgr *manager.Manager) error {
	externalEtcdCerts := []string{"external-etcd-ca.crt", "external-etcd.crt", "external-etcd.key"}
	for _, cert := range externalEtcdCerts {
		_, err := mgr.Runner.RunCmd(fmt.Sprintf("/usr/local/bin/kubectl patch -n kube-system secret kubeadm-certs -p '{\"data\": {\"%s\": \"\"}}'", cert))
		if err != nil {
			return errors.Wrap(errors.WithStack(err), "failed to patch kubeadm secret")
		}
	}
	return nil
}

func JoinNodesToCluster(mgr *manager.Manager) error {
	mgr.Logger.Infoln("Join nodes to cluster")

	return mgr.RunTaskOnK8sNodes(joinNodesToCluster, true)
}

func joinNodesToCluster(mgr *manager.Manager, node *kubekeyapi.HostCfg, conn ssh.Connection) error {
	if !strings.Contains(clusterStatus["clusterInfo"], node.Name) && !strings.Contains(clusterStatus["clusterInfo"], node.InternalAddress) {
		if node.IsMaster {
			err := addMaster(mgr)
			if err != nil {
				return err
			}
			err1 := removeMasterTaint(mgr, node)
			if err1 != nil {
				return err1
			}
			err2 := addWorkerLabel(mgr, node)
			if err2 != nil {
				return err2
			}
		}
		if node.IsWorker && !node.IsMaster {
			err := addWorker(mgr)
			if err != nil {
				return err
			}
			err1 := addWorkerLabel(mgr, node)
			if err1 != nil {
				return err1
			}
		}
	}
	return nil
}

func addMaster(mgr *manager.Manager) error {
	_, err := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"%s\"", clusterStatus["joinMasterCmd"]))
	if err != nil {
		return errors.Wrap(errors.WithStack(err), "failed to add master to cluster")
	}
	err1 := GetKubeConfig(mgr)
	if err1 != nil {
		return err1
	}
	return nil
}

func addWorker(mgr *manager.Manager) error {
	_, err := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"%s\"", clusterStatus["joinWorkerCmd"]))
	if err != nil {
		return errors.Wrap(errors.WithStack(err), "failed to add worker to cluster")
	}
	createConfigDirCmd := "mkdir -p /root/.kube && mkdir -p $HOME/.kube"
	chownKubeConfig := "chown $(id -u):$(id -g) $HOME/.kube/config"
	_, err1 := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"%s\"", createConfigDirCmd))
	if err1 != nil {
		return errors.Wrap(errors.WithStack(err1), "failed to create kube dir")
	}
	syncKubeconfigCmd := fmt.Sprintf("echo %s | base64 -d > %s && echo %s | base64 -d > %s && %s", clusterStatus["kubeConfig"], "/root/.kube/config", clusterStatus["kubeConfig"], "$HOME/.kube/config", chownKubeConfig)
	_, err2 := mgr.Runner.RunCmd(fmt.Sprintf("sudo -E /bin/sh -c \"%s\"", syncKubeconfigCmd))
	if err2 != nil {
		return errors.Wrap(errors.WithStack(err2), "failed to sync kube config")
	}
	return nil
}
