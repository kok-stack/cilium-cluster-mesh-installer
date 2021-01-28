package main

import (
	"bytes"
	"fmt"
	v1 "github.com/kok-stack/kok/api/v1"
	"gopkg.in/yaml.v2"
	"html/template"
	"io/ioutil"
	v13 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os/exec"
	"path/filepath"
	"strconv"
)

/*
/etc/clusters/a/config
*/
const namespace = "kube-system"
const clusterDir = "/etc/clusters"
const configName = "config"
const ciliumDsName = "cilium"

func main() {
	files, err := ioutil.ReadDir(clusterDir)
	if err != nil {
		panic(err.Error())
	}
	clusters := make([]*clusterData, len(files))
	for i, file := range files {
		if !file.IsDir() {
			continue
		}
		name := file.Name()
		kubeconfig := filepath.Join(clusterDir, name, configName)
		data := &clusterData{
			KubeConfigPath: kubeconfig,
			Name:           name,
			Dir:            filepath.Join(clusterDir, name),
		}
		//heml安装cilium
		var err error
		err = ExecCommand(`helm repo add cilium https://helm.cilium.io/ --kubeconfig={{.KubeConfigPath}}`, data)
		if err != nil {
			panic(err.Error())
		}
		err = ExecCommand(`helm install cilium cilium/cilium --version 1.9.3 \
   --namespace kube-system \
   --set etcd.enabled=true \
   --set etcd.managed=true --kubeconfig={{.KubeConfigPath}}`, data)
		if err != nil {
			panic(err.Error())
		}
		client, err := getClientForCluster(kubeconfig)
		if err != nil {
			panic(err.Error())
		}
		//编辑cm cilium-config
		cm, err := client.CoreV1().ConfigMaps(namespace).Get("cilium-config", v12.GetOptions{})
		if err != nil {
			panic(err.Error())
		}
		cm.Data["cluster-name"] = name
		cm.Data["cluster-id"] = strconv.Itoa(i)

		_, err = client.CoreV1().ConfigMaps(namespace).Update(cm)
		if err != nil {
			panic(err.Error())
		}

		etcdService := &v13.Service{
			ObjectMeta: v12.ObjectMeta{
				Name:      "cilium-etcd-external",
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
		//执行./extract-etcd-secrets.sh generate-secret-yaml.sh generate-name-mapping.sh
		err = ExecCommand(`sh /extract-etcd-secrets.sh`, data)
		if err != nil {
			panic(err.Error())
		}
		err = ExecCommand(`sh /generate-secret-yaml.sh > clustermesh.yaml`, data)
		if err != nil {
			panic(err.Error())
		}
		err = ExecCommand(`sh /generate-name-mapping.sh > ds.patch`, data)
		if err != nil {
			panic(err.Error())
		}
		data.DsData, err = ioutil.ReadFile(filepath.Join(data.Dir, "ds.patch"))
		if err != nil {
			panic(err.Error())
		}
		clustermeshData, err := ioutil.ReadFile(filepath.Join(data.Dir, "clustermesh.yaml"))
		if err != nil {
			panic(err.Error())
		}
		err = yaml.Unmarshal(clustermeshData, data)
		if err != nil {
			panic(err.Error())
		}
		//得到ds.patch和 clustermesh.yaml
		clusters[i] = data
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
		_, err = forCluster.AppsV1().DaemonSets(namespace).Patch(ciliumDsName, types.StrategicMergePatchType, patch)
		if err != nil {
			println(err.Error())
		}
		//kubectl -n kube-system apply -f clustermesh.yaml
		_, err = forCluster.CoreV1().Secrets(namespace).Create(secret)
		if err != nil {
			panic(err.Error())
		}

		//kubectl -n kube-system delete pod -l k8s-app=cilium
		ciliumList, err := forCluster.CoreV1().Pods(namespace).List(v12.ListOptions{
			LabelSelector: "k8s-app=cilium",
		})
		if err != nil {
			panic(err.Error())
		}
		for _, item := range ciliumList.Items {
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
			_ = forCluster.CoreV1().Pods(namespace).Delete(item.Name, &v12.DeleteOptions{})
		}
	}

	//ClusterRest()

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

func mergePatch(clusters []*clusterData) []byte {
	result := &DsData{}
	aliases := make([]hostAliase, len(clusters))

	for i, cluster := range clusters {
		tmp := &DsData{}
		if err := yaml.Unmarshal(cluster.DsData, &tmp); err != nil {
			panic(err)
		}
		aliases[i] = tmp.Spec.Template.Spec.HostAliases[0]
	}
	result.Spec.Template.Spec.HostAliases = aliases
	marshal, err := yaml.Marshal(result)
	if err != nil {
		panic(err.Error())
	}
	return marshal
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
			data[s] = []byte(s2)
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

func ExecCommand(cmd string, data *clusterData, arg ...string) error {
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
	fmt.Println("执行命令:", targetCmd, "参数:", arg)
	command := exec.Command(targetCmd, arg...)
	command.Env = append(command.Env, "KUBECONFIG", data.KubeConfigPath)
	command.Dir = data.Dir
	output, err := command.CombinedOutput()
	if err != nil {
		return err
	}
	fmt.Println("输出:", string(output))
	return nil
}

func ClusterRest() {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	configOverrides := &clientcmd.ConfigOverrides{}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	config, err := kubeConfig.ClientConfig()
	if err != nil {
		panic(err)
	}

	fmt.Println("test...")
	cluster := &v1.Cluster{}
	config.APIPath = "/apis"
	config.GroupVersion = &schema.GroupVersion{
		Group:   "cluster.kok.tanx",
		Version: "v1",
	}
	config.NegotiatedSerializer = scheme.Codecs
	forConfig, err := rest.RESTClientFor(config)
	if err != nil {
		panic(err)
	}
	err = forConfig.Get().Namespace("test").Resource("clusters").Name("test").Do().Into(cluster)
	if err != nil {
		found := errors.IsNotFound(err)
		fmt.Println("found:", found)
		print(err)
	}

	fmt.Println(cluster.Status)
}
