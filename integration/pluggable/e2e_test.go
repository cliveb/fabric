/*
Copyright IBM Corp All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package e2e

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/hyperledger/fabric/integration/world"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/tedsuo/ifrit"
)

var _ = Describe("EndToEnd", func() {
	var (
		client     *docker.Client
		network    *docker.Network
		w          world.World
		deployment world.Deployment
	)

	BeforeEach(func() {
		var err error

		client, err = docker.NewClientFromEnv()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		// Stop the docker constainers for zookeeper and kafka
		for _, cont := range w.LocalStoppers {
			cont.Stop()
		}

		// Stop the running chaincode containers
		filters := map[string][]string{}
		filters["name"] = []string{fmt.Sprintf("%s-%s", deployment.Chaincode.Name, deployment.Chaincode.Version)}
		allContainers, _ := client.ListContainers(docker.ListContainersOptions{
			Filters: filters,
		})
		if len(allContainers) > 0 {
			for _, container := range allContainers {
				client.RemoveContainer(docker.RemoveContainerOptions{
					ID:    container.ID,
					Force: true,
				})
			}
		}

		// Remove chaincode image
		filters = map[string][]string{}
		filters["label"] = []string{fmt.Sprintf("org.hyperledger.fabric.chaincode.id.name=%s", deployment.Chaincode.Name)}
		images, _ := client.ListImages(docker.ListImagesOptions{
			Filters: filters,
		})
		if len(images) > 0 {
			for _, image := range images {
				client.RemoveImage(image.ID)
			}
		}

		// Stop the orderers and peers
		for _, localProc := range w.LocalProcess {
			localProc.Signal(syscall.SIGTERM)
		}

		// Remove any started networks
		if network != nil {
			client.RemoveNetwork(network.Name)
		}
	})

	It("executes a basic solo network with specified plugins", func() {
		By("generating a basic config")
		w = world.GenerateBasicConfig("solo", 1, 2, testDir, components)

		deployment = world.Deployment{
			Channel: "testchannel",
			Chaincode: world.Chaincode{
				Name:     "mycc",
				Version:  "0.0",
				Path:     filepath.Join("github.com", "hyperledger", "fabric", "integration", "chaincode", "simple", "cmd"),
				ExecPath: os.Getenv("PATH"),
			},
			InitArgs: `{"Args":["init","a","100","b","200"]}`,
			Policy:   `OR ('Org1MSP.member','Org2MSP.member')`,
			Orderer:  "127.0.0.1:7050",
		}

		By("setting up the network")
		w.SetupWorld(deployment)

		// count peers
		peerCount := 0
		for _, peerOrg := range w.PeerOrgs {
			peerCount += peerOrg.PeerCount
		}
		// Make sure plugins activated
		activations := CountEndorsementPluginActivations()
		Expect(activations).To(Equal(peerCount))
		activations = CountValidationPluginActivations()
		Expect(activations).To(Equal(peerCount))

		By("querying the chaincode")
		adminPeer := components.Peer()
		adminPeer.LogLevel = "debug"
		adminPeer.ConfigDir = filepath.Join(testDir, "peer0.org1.example.com")
		adminPeer.MSPConfigPath = filepath.Join(testDir, "crypto", "peerOrganizations", "org1.example.com", "users", "Admin@org1.example.com", "msp")
		adminRunner := adminPeer.QueryChaincode(deployment.Chaincode.Name, deployment.Channel, `{"Args":["query","a"]}`)
		execute(adminRunner)
		Eventually(adminRunner.Buffer()).Should(gbytes.Say("100"))

		By("invoking the chaincode")
		adminRunner = adminPeer.InvokeChaincode(deployment.Chaincode.Name, deployment.Channel, `{"Args":["invoke","a","b","10"]}`, deployment.Orderer)
		execute(adminRunner)
		Eventually(adminRunner.Err()).Should(gbytes.Say("Chaincode invoke successful. result: status:200"))

		By("querying the chaincode again")
		adminRunner = adminPeer.QueryChaincode(deployment.Chaincode.Name, deployment.Channel, `{"Args":["query","a"]}`)
		execute(adminRunner)
		Eventually(adminRunner.Buffer()).Should(gbytes.Say("90"))

		By("updating the channel")
		adminPeer = components.Peer()
		adminPeer.ConfigDir = filepath.Join(testDir, "peer0.org1.example.com")
		adminPeer.MSPConfigPath = filepath.Join(testDir, "crypto", "peerOrganizations", "org1.example.com", "users", "Admin@org1.example.com", "msp")
		adminRunner = adminPeer.UpdateChannel(filepath.Join(testDir, "Org1_anchors_update_tx.pb"), deployment.Channel, deployment.Orderer)
		execute(adminRunner)
		Eventually(adminRunner.Err()).Should(gbytes.Say("Successfully submitted channel update"))
	})
})

func copyFile(src, dest string) {
	data, err := ioutil.ReadFile(src)
	Expect(err).NotTo(HaveOccurred())
	err = ioutil.WriteFile(dest, data, 0775)
	Expect(err).NotTo(HaveOccurred())
}

func execute(r ifrit.Runner) (err error) {
	p := ifrit.Invoke(r)
	Eventually(p.Ready()).Should(BeClosed())
	Eventually(p.Wait(), 10*time.Second).Should(Receive(&err))
	return err
}

func copyPeerConfigs(peerOrgs []world.PeerOrgConfig, rootPath string) {
	for _, peerOrg := range peerOrgs {
		for peer := 0; peer < peerOrg.PeerCount; peer++ {
			peerDir := fmt.Sprintf("%s_%d", peerOrg.Domain, peer)
			if _, err := os.Stat(filepath.Join(rootPath, peerDir)); os.IsNotExist(err) {
				err := os.Mkdir(filepath.Join(rootPath, peerDir), 0755)
				Expect(err).NotTo(HaveOccurred())
			}
			copyFile(filepath.Join("testdata", fmt.Sprintf("%s-core.yaml", peerDir)),
				filepath.Join(rootPath, peerDir, "core.yaml"))
		}
	}
}

// compilePlugin compiles the plugin of the given type and returns the path for the plugin file
func compilePlugin(pluginType string) string {
	pluginFilePath := filepath.Join("testdata", "plugins", pluginType, "plugin.so")
	cmd := exec.Command(
		"go",
		append([]string{
			"build", "-buildmode=plugin", "-o", pluginFilePath,
			fmt.Sprintf("github.com/hyperledger/fabric/integration/pluggable/testdata/plugins/%s", pluginType),
		})...,
	)
	cmd.Run()
	Expect(pluginFilePath).To(BeARegularFile())
	return pluginFilePath
}