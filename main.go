package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"gopkg.in/yaml.v2"
	"html/template"
	"io/ioutil"
	v13 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

/*
/etc/clusters/a/config
*/
const namespace = "kube-system"
const clusterDir = "/etc/cluster"
const configName = "config"
const ciliumDsName = "cilium"

func main() {
	files, err := ioutil.ReadDir(clusterDir)
	if err != nil {
		panic(err.Error())
	}
	clusters := make([]*clusterData, 0)
	for _, file := range files {
		name := file.Name()
		//if !file.IsDir() {
		//	fmt.Printf("%s不是文件夹，跳过此文件 \n", name)
		//	continue
		//}
		if strings.HasPrefix(name, "..") {
			fmt.Printf("%s以..开头，跳过此文件夹 \n", name)
			continue
		}
		fmt.Printf("[%s]开始安装\n", name)
		kubeconfig := filepath.Join(clusterDir, name, configName)
		data := &clusterData{
			KubeConfigPath: kubeconfig,
			Name:           name,
			Dir:            filepath.Join(clusterDir, name),
		}
		//heml安装cilium
		var err error
		fmt.Printf("[%s]添加helm仓库\n", name)
		err = ExecCommand(`/usr/local/bin/helm repo add cilium https://helm.cilium.io/ --kubeconfig={{.KubeConfigPath}}`, data)
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("[%s]helm 安装 cilium\n", name)
		err = ExecCommand(`/usr/local/bin/helm install cilium cilium/cilium --version 1.9.3 \
   --namespace kube-system \
   --set etcd.enabled=true \
   --set etcd.managed=true --kubeconfig={{.KubeConfigPath}} --replace`, data)
		if err != nil {
			panic(err.Error())
		}
		client, err := getClientForCluster(kubeconfig)
		if err != nil {
			panic(err.Error())
		}

		for i := 0; i < 2000; i++ {
			ciliumList, _ := client.CoreV1().Pods(namespace).List(v12.ListOptions{
				LabelSelector: "k8s-app=cilium",
			})
			fmt.Printf("[%s][%v/2000]获取pod状态为", name, i)
			for _, item := range ciliumList.Items {
				fmt.Print("pod:", item.Name, " ")

				for _, condition := range item.Status.Conditions {
					ready := condition.Type == v13.PodReady && condition.Status == v13.ConditionTrue
					fmt.Print(condition.Type, ":", condition.Status, " ready:", ready, " ")
					if ready {
						goto label1
					}
				}
			}
			fmt.Println("")
			fmt.Printf("[%s][%v/2000]获取pod状态,未ready,等待1s重试\n", name, i)
			time.Sleep(time.Second)
		}
	label1:

		//编辑cm cilium-config
		fmt.Printf("[%s]获取configmap\n", name)
		cmName := "cilium-config"
		for index := 0; index < 10; index++ {
			cm, err := client.CoreV1().ConfigMaps(namespace).Get(cmName, v12.GetOptions{})
			if err != nil {
				if errors.IsNotFound(err) {
					fmt.Printf("[%s][%v/10]未找到configmap:%s,等待2s后重试\n", name, index, cmName)
					time.Sleep(time.Second * 2)
					continue
				} else {
					panic(err.Error())
				}
			}
			cm.Data["cluster-name"] = name
			cm.Data["cluster-id"] = strconv.Itoa(index + 1)

			fmt.Printf("[%s]更新configmap\n", name)
			_, err = client.CoreV1().ConfigMaps(namespace).Update(cm)
			if err != nil {
				panic(err.Error())
			} else {
				fmt.Printf("[%s]已更新configmap(%s),cluster-id:%s,cluster-name:%s \n", name, cmName, cm.Data["cluster-id"], cm.Data["cluster-name"])
			}
			break
		}

		n := "cilium-etcd-external"
		_, err = client.CoreV1().Services(namespace).Get(n, v12.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				fmt.Printf("[%s]未找到svc,开始创建\n", name)
				etcdService := &v13.Service{
					ObjectMeta: v12.ObjectMeta{
						Name:      n,
						Namespace: namespace,
					},
					Spec: v13.ServiceSpec{
						Type: v13.ServiceTypeNodePort,
						Ports: []v13.ServicePort{
							{
								Port: 2379,
							},
						},
						Selector: map[string]string{
							"app":           "etcd",
							"etcd_cluster":  "cilium-etcd",
							"io.cilium/app": "etcd-operator",
						},
					},
				}
				//创建svc
				_, err = client.CoreV1().Services(namespace).Create(etcdService)
				if err != nil {
					panic(err.Error())
				}
				fmt.Printf("[%s]未找到svc,创建完成\n", name)
			} else {
				fmt.Printf("[%s]未找到svc,创建出现错误\n", name)
				panic(err.Error())
			}
		}

		fmt.Printf("[%s]执行脚本,获取etcd secret\n", name)
		//执行./extract-etcd-secrets.sh generate-secret-yaml.sh generate-name-mapping.sh
		err = ExecCommand(`export KUBECONFIG={{.KubeConfigPath}} && export OUTPUT={{.Dir}}/config && /extract-etcd-secrets.sh`, data)
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("[%s]执行脚本,生成yaml\n", name)
		err = ExecCommand(`export KUBECONFIG={{.KubeConfigPath}} && export OUTPUT={{.Dir}}/config && /generate-secret-yaml.sh > {{.Dir}}/clustermesh.yaml`, data)
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("[%s]执行脚本,生成ds.patch\n", name)
		err = ExecCommand(`export KUBECONFIG={{.KubeConfigPath}} && export OUTPUT={{.Dir}}/config && /generate-name-mapping.sh > {{.Dir}}/ds.patch`, data)
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("[%s]读取ds.patch\n", name)
		data.DsData, err = ioutil.ReadFile(filepath.Join(data.Dir, "ds.patch"))
		if err != nil {
			panic(err.Error())
		}
		fmt.Printf("[%s]读取clustermesh.yaml\n", name)
		clustermeshData, err := ioutil.ReadFile(filepath.Join(data.Dir, "clustermesh.yaml"))
		if err != nil {
			panic(err.Error())
		}
		err = yaml.Unmarshal(clustermeshData, data)
		if err != nil {
			panic(err.Error())
		}
		//得到ds.patch和 clustermesh.yaml
		clusters = append(clusters, data)
	}
	//合并clustermesh.yaml
	secret := mergeSecret(clusters)
	patch := mergePatch(clusters)
	//在集群循环执行
	for _, cluster := range clusters {
		forCluster, err := getClientForCluster(cluster.KubeConfigPath)
		if err != nil {
			panic(err.Error())
		}
		//kubectl -n kube-system patch ds cilium -p "$(cat ds.patch)"
		fmt.Printf("[%s]更新DaemonSet\n", cluster.Name)
		ciliumDs, err := forCluster.AppsV1().DaemonSets(namespace).Get(ciliumDsName, v12.GetOptions{})
		if err != nil {
			println(err.Error())
		}

		aliases := make([]v13.HostAlias, len(patch))
		for i, aliase := range patch {
			aliases[i] = v13.HostAlias{
				IP:        aliase.Ip,
				Hostnames: aliase.Hostnames,
			}
		}
		ciliumDs.Spec.Template.Spec.HostAliases = aliases
		for _, alias := range ciliumDs.Spec.Template.Spec.HostAliases {
			fmt.Println(alias.IP, alias.Hostnames)
		}
		_, err = forCluster.AppsV1().DaemonSets(namespace).Update(ciliumDs)
		if err != nil {
			println(err.Error())
		}

		fmt.Printf("[%s]创建secret\n", cluster.Name)
		//kubectl -n kube-system apply -f clustermesh.yaml
		_, err = forCluster.CoreV1().Secrets(namespace).Create(secret)
		if err != nil {
			panic(err.Error())
		}

		time.Sleep(time.Second * 10)
		fmt.Printf("[%s]获取pod,并重启\n", cluster.Name)
		//kubectl -n kube-system delete pod -l k8s-app=cilium
		ciliumList, err := forCluster.CoreV1().Pods(namespace).List(v12.ListOptions{
			LabelSelector: "k8s-app=cilium",
		})
		if err != nil {
			panic(err.Error())
		}
		for _, item := range ciliumList.Items {
			fmt.Println("删除pod", item.Name)
			_ = forCluster.CoreV1().Pods(namespace).Delete(item.Name, &v12.DeleteOptions{})
		}

		//kubectl -n kube-system delete pod -l name=cilium-operator
		operatorList, err := forCluster.CoreV1().Pods(namespace).List(v12.ListOptions{
			LabelSelector: "name=cilium-operator",
		})
		if err != nil {
			panic(err.Error())
		}
		for _, item := range operatorList.Items {
			fmt.Println("删除pod", item.Name)
			_ = forCluster.CoreV1().Pods(namespace).Delete(item.Name, &v12.DeleteOptions{})
		}

		time.Sleep(time.Second * 5)
		//输出集群状态 kubectl -n kube-system exec -ti cilium-g6btl -- cilium node list
		var podName string
		for i := 0; i < 100; i++ {
			time.Sleep(time.Second * 1)
			ciliumList, _ = forCluster.CoreV1().Pods(namespace).List(v12.ListOptions{
				LabelSelector: "k8s-app=cilium",
			})
			if len(ciliumList.Items) > 0 {
				podName = ciliumList.Items[0].Name
				break
			}
		}
		if podName != "" {
			_ = ExecCommand("kubectl --kubeconfig={{.KubeConfigPath}} exec -t -n kube-system "+podName+" -- cilium node list", cluster)
		} else {
			fmt.Println("未获取到pod，请手动运行命令查看状态：kubectl exec -t -n kube-system 容器名称 -- cilium node list")
		}
	}

}

type hostAliase struct {
	Ip        string   `yaml:"ip,omitempty"`
	Hostnames []string `yaml:"hostnames,omitempty"`
}
type DsData struct {
	Spec struct {
		Template struct {
			Spec struct {
				HostAliases []hostAliase `yaml:"hostAliases,omitempty"`
			} `yaml:"spec,omitempty"`
		} `yaml:"template,omitempty"`
	} `yaml:"spec,omitempty"`
}

func mergePatch(clusters []*clusterData) []hostAliase {
	aliases := make([]hostAliase, len(clusters))

	for i, cluster := range clusters {
		tmp := &DsData{}
		if err := yaml.Unmarshal(cluster.DsData, &tmp); err != nil {
			panic(err)
		}
		aliases[i] = tmp.Spec.Template.Spec.HostAliases[0]
	}
	return aliases
}

func getClientForCluster(kubeconfig string) (*kubernetes.Clientset, error) {
	readFile, err := ioutil.ReadFile(kubeconfig)
	if err != nil {
		panic(err.Error())
	}
	config, err := clientcmd.NewClientConfigFromBytes(readFile)
	if err != nil {
		panic(err.Error())
	}
	clientConfig, err := config.ClientConfig()
	if err != nil {
		panic(err.Error())
	}
	client, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		panic(err.Error())
	}
	return client, nil
}

func mergeSecret(clusters []*clusterData) *v13.Secret {
	data := make(map[string][]byte)
	secret := &v13.Secret{
		ObjectMeta: v12.ObjectMeta{
			Namespace: namespace,
			Name:      "cilium-clustermesh",
		},
		Data: data,
	}
	for _, cluster := range clusters {
		for s, s2 := range cluster.Data {
			decodeString, err := base64.StdEncoding.DecodeString(s2)
			if err != nil {
				panic(err.Error())
			}
			data[s] = decodeString
		}
	}
	return secret
}

type clusterData struct {
	KubeConfigPath string
	Name           string
	Dir            string
	DsData         []byte
	Data           map[string]string `yaml:"data,omitempty"`
}

func ExecCommand(cmd string, data *clusterData) error {
	parse, err := template.New("t").Parse(cmd)
	if err != nil {
		return err
	}
	buffer := &bytes.Buffer{}
	err = parse.Execute(buffer, data)
	if err != nil {
		return err
	}
	targetCmd := buffer.String()
	fmt.Println("执行命令:", targetCmd)
	command := exec.Command("/bin/sh", "-c", targetCmd)
	command.Env = append(command.Env, "KUBECONFIG", data.KubeConfigPath)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}
