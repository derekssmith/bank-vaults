// Copyright © 2019 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package registry

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"strings"

	dockerTypes "github.com/docker/docker/api/types"
	"github.com/heroku/docker-registry-client/registry"
	imagev1 "github.com/opencontainers/image-spec/specs-go/v1"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var logger log.FieldLogger

func init() {
	logger = log.New()
}

type DockerCreds struct {
	Auths map[string]dockerTypes.AuthConfig `json:"auths"`
}

// GetImageBlob download image blob from registry
func GetImageBlob(url, username, password, image string) ([]string, []string, error) {
	imageName, tag, err := ParseContainerImage(image)
	if err != nil {
		return nil, nil, err
	}

	registrySkipVerify := os.Getenv("REGISTRY_SKIP_VERIFY")

	var hub *registry.Registry

	if registrySkipVerify == "true" {
		hub, err = registry.NewInsecure(url, username, password)
	} else {
		hub, err = registry.New(url, username, password)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("cannot create client for registry: %s", err.Error())
	}

	manifest, err := hub.ManifestV2(imageName, tag)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot download manifest for image: %s", err.Error())
	}

	reader, err := hub.DownloadBlob(imageName, manifest.Config.Digest)
	if reader != nil {
		defer reader.Close()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("cannot download blob: %s", err.Error())
	}

	b, err := ioutil.ReadAll(reader)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read blob: %s", err.Error())
	}

	logger.Info("downloaded blob len: ", len(b))

	var imageMetadata imagev1.Image
	err = json.Unmarshal(b, &imageMetadata)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot unmarshal BlobResponse JSON: %s", err.Error())
	}

	return imageMetadata.Config.Entrypoint, imageMetadata.Config.Cmd, nil
}

// ParseContainerImage returns image and tag
func ParseContainerImage(image string) (string, string, error) {
	split := strings.SplitN(image, ":", 2)

	if len(split) <= 1 {
		return "", "", fmt.Errorf("Cannot find tag for image %s", image)
	}

	imageName := split[0]
	tag := split[1]

	return imageName, tag, nil
}

func isDockerHub(registryAddress string) bool {
	return strings.HasPrefix(registryAddress, "https://registry-1.docker.io") || strings.HasPrefix(registryAddress, "https://index.docker.io")
}

// GetEntrypointCmd returns entrypoint and command of container
func GetEntrypointCmd(clientset *kubernetes.Clientset, namespace string, container *corev1.Container, podSpec *corev1.PodSpec) ([]string, []string, error) {
	podInfo := K8s{Namespace: namespace, clientset: clientset}

	err := podInfo.Load(container, podSpec)
	if err != nil {
		return nil, nil, err
	}

	if podInfo.RegistryName != "" {
		logger.Info(
			"Trimmed registry name from image name",
			"registry", podInfo.RegistryName,
			"image", podInfo.Image,
		)
		podInfo.Image = strings.TrimLeft(podInfo.Image, fmt.Sprintf("%s/", podInfo.RegistryName))
	}

	registryAddress := podInfo.RegistryAddress
	if registryAddress == "" {
		registryAddress = "https://registry-1.docker.io/"
	}

	// this is a library image on DockerHub, add the `libarary/` prefix
	if isDockerHub(registryAddress) && strings.Count(podInfo.Image, "/") == 0 {
		podInfo.Image = "library/" + podInfo.Image
	}

	logger.Infoln("I'm using registry", registryAddress)

	return GetImageBlob(registryAddress, podInfo.RegistryUsername, podInfo.RegistryPassword, podInfo.Image)
}

// K8s structure keeps information retrieved from POD definition
type K8s struct {
	clientset        *kubernetes.Clientset
	Namespace        string
	ImagePullSecrets string
	RegistryAddress  string
	RegistryName     string
	RegistryUsername string
	RegistryPassword string
	Image            string
}

func (k *K8s) readDockerSecret(namespace, secretName string) (map[string][]byte, error) {
	secret, err := k.clientset.CoreV1().Secrets(namespace).Get(secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return secret.Data, nil
}

func (k *K8s) parseDockerConfig(dockerCreds DockerCreds) {
	k.RegistryName = reflect.ValueOf(dockerCreds.Auths).MapKeys()[0].String()
	if !strings.HasPrefix(k.RegistryName, "https://") {
		k.RegistryAddress = fmt.Sprintf("https://%s", k.RegistryName)
	} else {
		k.RegistryAddress = k.RegistryName
	}

	auths := dockerCreds.Auths
	k.RegistryUsername = auths[k.RegistryName].Username
	k.RegistryPassword = auths[k.RegistryName].Password
}

// Load reads information from k8s and load them into the structure
func (k *K8s) Load(container *corev1.Container, podSpec *corev1.PodSpec) error {

	k.Image = container.Image

	if len(podSpec.ImagePullSecrets) >= 1 {
		k.ImagePullSecrets = podSpec.ImagePullSecrets[0].Name

		if k.ImagePullSecrets != "" {
			data, err := k.readDockerSecret(k.Namespace, k.ImagePullSecrets)
			if err != nil {
				return fmt.Errorf("cannot read imagePullSecrets: %s", err.Error())
			}

			dockerConfig := data[corev1.DockerConfigJsonKey]

			var dockerCreds DockerCreds
			err = json.Unmarshal(dockerConfig, &dockerCreds)
			if err != nil {
				return fmt.Errorf("cannot unmarshal docker configuration from imagePullSecrets: %s", err.Error())
			}
			k.parseDockerConfig(dockerCreds)
		}
	}

	return nil
}