/*
Copyright © 2023 - 2024 SUSE LLC

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

package e2e_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	. "github.com/rancher-sandbox/qase-ginkgo"
	"github.com/rancher/norman/types"
	rancher "github.com/rancher/shepherd/clients/rancher"
	management "github.com/rancher/shepherd/clients/rancher/generated/management/v3"
	clusters "github.com/rancher/shepherd/extensions/clusters"
	session "github.com/rancher/shepherd/pkg/session"
	e2e "k8s.io/kubernetes/test/e2e/framework"
)

var (
	client          *rancher.Client
	sess            *session.Session
	project         *management.Project
	cluster         *clusters.ClusterMeta
	registrySetting *management.Setting
	err             error
	testCaseID      int64
)

var _ = ReportAfterEach(func(report SpecReport) {
	if testCaseID == 0 {
		return
	}
	// Add result in Qase if asked
	Qase(testCaseID, report)

	// This prevents the ID from "leaking" into the next test's ReportAfterEach execution.
	testCaseID = 0
})

func FailWithReport(message string, callerSkip ...int) {
	// Ensures the correct line numbers are reported
	Fail(message, callerSkip[0]+1)
}

// Run individual or group of tests with labels using CLI
// TEST_LABEL_FILTER=monitoring  /usr/local/go/bin/go test -timeout 60m github.com/rancher/observability-e2e/tests/e2e -v -count=1 -ginkgo.v
func TestE2E(t *testing.T) {
	RegisterFailHandler(FailWithReport)
	suiteConfig, reporterConfig := GinkgoConfiguration()

	// Set the label filter to "LEVEL0" (or any other test with custom tag)
	if envLabelFilter := os.Getenv("TEST_LABEL_FILTER"); envLabelFilter != "" {
		suiteConfig.LabelFilter = envLabelFilter
	} else {
		suiteConfig.LabelFilter = "LEVEL0"
	}
	e2e.Logf("Executing tests with label '%v'", suiteConfig.LabelFilter)
	RunSpecs(t, "Observability End-To-End Test Suite", suiteConfig, reporterConfig)
}

// This setup will run once for the entire test suite
var _ = BeforeSuite(func() {
	project = nil

	testSession := session.NewSession()
	sess = testSession

	client, err = rancher.NewClient("", testSession)
	Expect(err).NotTo(HaveOccurred())

	// Get clusterName from config yaml
	clusterName := client.RancherConfig.ClusterName
	Expect(clusterName).NotTo(BeEmpty(), "Cluster name to install is not set")

	// Get cluster meta
	cluster, err = clusters.NewClusterMeta(client, clusterName)
	Expect(err).NotTo(HaveOccurred())

	// Get Server and Registry Setting Values
	registrySetting, err = client.Management.Setting.ByID("system-default-registry")
	Expect(err).NotTo(HaveOccurred())

	projectsList, err := client.Management.Project.List(&types.ListOpts{
		Filters: map[string]interface{}{
			"clusterId": cluster.ID,
		},
	})
	Expect(err).NotTo(HaveOccurred())

	for i := range projectsList.Data {
		p := &projectsList.Data[i]
		if p.Name == exampleAppProjectName {
			project = p
			break
		}
	}

	// Check if project was found
	if project == nil {
		projectConfig := &management.Project{
			ClusterID: cluster.ID,
			Name:      exampleAppProjectName,
		}

		project, err = client.Management.Project.Create(projectConfig)
		Expect(err).NotTo(HaveOccurred())
		Expect(project.Name).To(Equal(exampleAppProjectName))
	}
})

// This teardown will run once after all the tests in the suite are done
var _ = AfterSuite(func() {
	sess.Cleanup()
})
