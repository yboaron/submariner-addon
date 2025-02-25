package azure

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	submreporter "github.com/submariner-io/admiral/pkg/reporter"

	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/pkg/errors"
	"github.com/stolostron/submariner-addon/pkg/cloud/reporter"
	"github.com/stolostron/submariner-addon/pkg/constants"
	"github.com/submariner-io/cloud-prepare/pkg/api"
	"github.com/submariner-io/cloud-prepare/pkg/azure"
	"github.com/submariner-io/cloud-prepare/pkg/k8s"
	"github.com/submariner-io/cloud-prepare/pkg/ocp"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const (
	servicePrincipalJSON = "osServicePrincipal.json"
	gwInstanceType       = "Standard_F4s_v2"
)

type azureProvider struct {
	infraID              string
	nattPort             uint16
	routePort            string
	gatewayMetricsPort   uint16
	globalnetMetricsPort uint16
	cloudPrepare         api.Cloud
	reporter             submreporter.Interface
	gwDeployer           api.GatewayDeployer
	gateways             int
	nattDiscoveryPort    int64
}

func NewAzureProvider(
	restMapper meta.RESTMapper,
	kubeClient kubernetes.Interface,
	dynamicClient dynamic.Interface,
	hubKubeClient kubernetes.Interface,
	eventRecorder events.Recorder,
	region, infraID, clusterName, credentialsSecretName, instanceType string,
	nattPort, nattDiscoveryPort, gateways int,
) (*azureProvider, error) {
	if infraID == "" {
		return nil, fmt.Errorf("cluster infraID is empty")
	}

	if nattPort == 0 {
		nattPort = constants.SubmarinerNatTPort
	}

	if instanceType == "" {
		instanceType = gwInstanceType
	}

	if gateways < 1 {
		return nil, fmt.Errorf("the count of gateways is less than 1")
	}

	if nattDiscoveryPort == 0 {
		nattDiscoveryPort = constants.SubmarinerNatTDiscoveryPort
	}

	subscriptionID, err := initializeFromAuthFile(hubKubeClient, clusterName, credentialsSecretName)
	if err != nil {
		return nil, errors.Wrap(err, "unable to initialize from auth file")
	}

	authorizer, err := auth.NewAuthorizerFromEnvironment()

	if err != nil {
		return nil, errors.Wrap(err, "unable to create the Azure authorizer")
	}

	k8sClient := k8s.NewInterface(kubeClient)

	msDeployer := ocp.NewK8sMachinesetDeployer(restMapper, dynamicClient)

	cloudInfo := azure.CloudInfo{
		SubscriptionID: subscriptionID,
		InfraID:        infraID,
		Region:         region,
		BaseGroupName:  infraID + "-rg",
		Authorizer:     authorizer,
		K8sClient:      k8sClient,
	}

	azureCloud := azure.NewCloud(&cloudInfo)

	gwDeployer, err := azure.NewOcpGatewayDeployer(&cloudInfo, azureCloud, msDeployer, instanceType, true)
	if err != nil {
		return nil, errors.Wrap(err, "failed to initialise a GatewayDeployer config")
	}

	cloudPrepare := azure.NewCloud(&cloudInfo)

	return &azureProvider{
		infraID:              infraID,
		nattPort:             uint16(nattPort),
		routePort:            strconv.Itoa(constants.SubmarinerRoutePort),
		gatewayMetricsPort:   constants.SubmarinerGatewayMetricsPort,
		globalnetMetricsPort: constants.SubmarinerGlobalnetMetricsPort,
		cloudPrepare:         cloudPrepare,
		gwDeployer:           gwDeployer,
		reporter:             reporter.NewEventRecorderWrapper("AzureCloudProvider", eventRecorder),
		nattDiscoveryPort:    int64(nattDiscoveryPort),
		gateways:             gateways,
	}, nil
}

// PrepareSubmarinerClusterEnv prepares submariner cluster environment on Azure
// The below tasks will be executed
// 1. create the inbound and outbound firewall rules for submariner, below ports will be opened
//    - NAT traversal port (by default 4500/UDP)
//    - 4800/UDP port to encapsulate Pod traffic from worker and master nodes to the Submariner Gateway nodes
//    - ESP & AH protocols for private-ip to private-ip gateway communications
// 2. create the inbound and outbound firewall rules to open 8080/TCP, 8081/TCP port to export metrics service from the
//    Submariner gateway
func (r *azureProvider) PrepareSubmarinerClusterEnv() error {
	//TODO For ovn the port 4800 need not be opened.
	if err := r.gwDeployer.Deploy(api.GatewayDeployInput{
		PublicPorts: []api.PortSpec{
			{Port: r.nattPort, Protocol: "udp"},
			{Port: uint16(r.nattDiscoveryPort), Protocol: "udp"},
			{Port: 0, Protocol: "esp"},
			{Port: 0, Protocol: "ah"},
		},
		Gateways: r.gateways,
	}, r.reporter); err != nil {
		return err
	}

	input := api.PrepareForSubmarinerInput{
		InternalPorts: []api.PortSpec{
			{Port: constants.SubmarinerRoutePort, Protocol: "udp"},
			{Port: r.gatewayMetricsPort, Protocol: "tcp"},
			{Port: r.globalnetMetricsPort, Protocol: "tcp"},
		},
	}
	err := r.cloudPrepare.PrepareForSubmariner(input, r.reporter)
	if err != nil {
		return err
	}

	r.reporter.Success("The Submariner cluster environment has been set up on Azure")
	return nil
}

// CleanUpSubmarinerClusterEnv clean up submariner cluster environment on Azure after the SubmarinerConfig was deleted
// 1. delete any dedicated gateways that were previously deployed.
// 2. delete the inbound and outbound firewall rules to close submariner ports
// 3. delete the inbound and outbound firewall rules to close submariner metrics port
func (r azureProvider) CleanUpSubmarinerClusterEnv() error {
	err := r.gwDeployer.Cleanup(r.reporter)
	if err != nil {
		return err
	}

	err = r.cloudPrepare.CleanupAfterSubmariner(r.reporter)
	if err != nil {
		return err
	}

	r.reporter.Success("The Submariner cluster environment has been cleaned up on Azure")
	return nil
}

func initializeFromAuthFile(kubeClient kubernetes.Interface, secretNamespace, secretName string) (string, error) {
	credentialsSecret, err := kubeClient.CoreV1().Secrets(secretNamespace).Get(context.TODO(), secretName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}

	servicePrincipalJSON, ok := credentialsSecret.Data[servicePrincipalJSON]
	var authInfo struct {
		ClientId       string
		ClientSecret   string
		SubscriptionId string
		TenantId       string
	}

	if !ok {
		return "", errors.New("servicePrincipalJSON is not found in the credentials")
	}

	err = json.Unmarshal(servicePrincipalJSON, &authInfo)
	if err != nil {
		return "", errors.Wrap(err, "error unmarshalling servicePrincipalJSON")
	}

	if err = os.Setenv("AZURE_CLIENT_ID", authInfo.ClientId); err != nil {
		return "", err
	}

	if err = os.Setenv("AZURE_CLIENT_SECRET", authInfo.ClientSecret); err != nil {
		return "", err
	}

	if err = os.Setenv("AZURE_TENANT_ID", authInfo.TenantId); err != nil {
		return "", err
	}

	return authInfo.SubscriptionId, nil
}
