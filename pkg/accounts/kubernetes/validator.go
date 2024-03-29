package kubernetes

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"

	tools "github.com/armory/go-yaml-tools/pkg/secrets"
	"github.com/armory/spinnaker-operator/pkg/apis/spinnaker/interfaces"
	"github.com/armory/spinnaker-operator/pkg/inspect"
	"github.com/armory/spinnaker-operator/pkg/secrets"
	"github.com/armory/spinnaker-operator/pkg/util"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	v13 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	clientcmdlatest "k8s.io/client-go/tools/clientcmd/api/latest"
	clientcmdv1 "k8s.io/client-go/tools/clientcmd/api/v1"
	certutil "k8s.io/client-go/util/cert"
	"k8s.io/klog"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	noAuthProvidedError      = fmt.Errorf("kubernetes auth needs to be defined")
	noKubernetesDefinedError = fmt.Errorf("kubernetes needs to be defined")
	noValidKubeconfigError   = fmt.Errorf("no valid kubeconfig file, kubeconfig content or service account information found")
	noServiceAccountName     = fmt.Errorf("no service account name configured in SpinnakerService for clouddriver")
)

type kubernetesAccountValidator struct {
	account *Account
}

func (k *kubernetesAccountValidator) Validate(spinSvc interfaces.SpinnakerService, c client.Client, ctx context.Context, log logr.Logger) error {
	if err := k.validateSettings(ctx, log); err != nil {
		return err
	}
	config, err := k.makeClient(ctx, spinSvc, c)
	if err != nil {
		return err
	}
	if config == nil {
		return nil
	}
	return k.validateAccess(ctx, config)
}

func (k *kubernetesAccountValidator) makeClient(ctx context.Context, spinSvc interfaces.SpinnakerService, c client.Client) (*rest.Config, error) {
	aSettings := authSettings{}
	if err := inspect.Source(&aSettings, k.account.Settings); err != nil {
		return nil, err
	}

	auth := k.account.Auth
	if auth == nil {
		// Attempt from settings
		return makeClientFromSettings(ctx, aSettings, spinSvc.GetSpinnakerConfig())
	}
	if auth.KubeconfigFile != "" {
		return makeClientFromFile(ctx, auth.KubeconfigFile, aSettings, spinSvc.GetSpinnakerConfig())
	}
	if auth.Kubeconfig != nil {
		// checking this
		return makeClientFromConfigAPI(auth.Kubeconfig, aSettings)
	}
	if auth.KubeconfigSecret != nil {
		return makeClientFromSecretRef(ctx, auth.KubeconfigSecret, aSettings)
	}
	if auth.UseServiceAccount {
		return makeClientFromServiceAccount(ctx, spinSvc, c)
	}
	return nil, noAuthProvidedError
}

// makeClientFromFile loads the client config from a file path which can be a secret
func makeClientFromFile(ctx context.Context, file string, settings authSettings, spinCfg *interfaces.SpinnakerConfig) (*rest.Config, error) {
	var cfg *clientcmdapi.Config
	var kubeconfigBytes []byte
	var err error
	if tools.IsEncryptedSecret(file) {
		f, err := secrets.DecodeAsFile(ctx, file)
		if err != nil {
			return nil, fmt.Errorf("error decoding kubeconfigFile from secret reference \"%s\":\n  %w", file, err)
		}
		kubeconfigBytes, err = ioutil.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("error loading kubeconfigFile \"%s\":\n  %w", f, err)
		}
	} else if filepath.IsAbs(file) {
		// if file path is absolute, it may already be a path decoded by secret engines
		kubeconfigBytes, err = ioutil.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("error loading kubeconfigFile \"%s\":\n  %w", file, err)
		}
	} else {
		// we're taking relative file paths as files defined inside spec.spinnakerConfig.files
		kubeconfigBytes = spinCfg.GetFileContent(file)
	}
	cfg, err = clientcmd.Load(kubeconfigBytes)
	if err != nil {
		return nil, fmt.Errorf("error parsing kubeconfigFile:\n  %w", err)
	}
	restCfg, err := clientcmd.NewDefaultClientConfig(*cfg, makeOverrideFromAuthSettings(cfg, settings)).ClientConfig()
	if err != nil {
		return restCfg, fmt.Errorf("error building rest config from kubeconfigFile:\n  %w", err)
	}
	return restCfg, nil
}

// makeClientFromSecretRef reads the client config from a Kubernetes secret in the current context's namespace
func makeClientFromSecretRef(ctx context.Context, ref *interfaces.SecretInNamespaceReference, settings authSettings) (*rest.Config, error) {
	sc, err := secrets.FromContextWithError(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "unable to make kubeconfig file")
	}
	str, err := util.GetSecretContent(sc.RestConfig, sc.Namespace, ref.Name, ref.Key)
	if err != nil {
		return nil, err
	}
	config, err := clientcmd.NewClientConfigFromBytes([]byte(str))
	if err != nil {
		return nil, err
	}

	cfg, err := config.RawConfig()
	if err != nil {
		return nil, fmt.Errorf("error parsing kubeconfigFile:\n  %w", err)
	}
	return clientcmd.NewDefaultClientConfig(cfg, makeOverrideFromAuthSettings(&cfg, settings)).ClientConfig()
}

// makeClientFromConfigAPI makes a client config from the v1 Config (the usual format for kubeconfig) inlined
// into the CRD.
func makeClientFromConfigAPI(config *clientcmdv1.Config, settings authSettings) (*rest.Config, error) {
	cfg := clientcmdapi.NewConfig()
	if err := clientcmdlatest.Scheme.Convert(config, cfg, nil); err != nil {
		return nil, nil
	}

	return clientcmd.NewDefaultClientConfig(*cfg, makeOverrideFromAuthSettings(cfg, settings)).ClientConfig()
}

// makeClientFromSettings makes a client config from Spinnaker settings
func makeClientFromSettings(ctx context.Context, aSettings authSettings, spinCfg *interfaces.SpinnakerConfig) (*rest.Config, error) {
	if aSettings.KubeconfigFile != "" {
		// this is checked
		return makeClientFromFile(ctx, aSettings.KubeconfigFile, aSettings, spinCfg)
	}
	if aSettings.KubeconfigContents != "" {
		cfg, err := clientcmd.Load([]byte(aSettings.KubeconfigContents))
		if err != nil {
			return nil, err
		}
		return clientcmd.NewDefaultClientConfig(*cfg, makeOverrideFromAuthSettings(cfg, aSettings)).ClientConfig()
	}
	return nil, noValidKubeconfigError
}

func makeClientFromServiceAccount(ctx context.Context, spinSvc interfaces.SpinnakerService, c client.Client) (*rest.Config, error) {
	spinSvc, err := ensureSpinSvc(spinSvc, c, ctx)
	if err != nil {
		return nil, err
	}
	an, err := spinSvc.GetSpinnakerConfig().GetServiceSettingsPropString(ctx, util.ClouddriverName, "kubernetes.serviceAccountName")
	if err != nil {
		return nil, noServiceAccountName
	}
	token, caPath, err := util.GetServiceAccountData(ctx, an, spinSvc.GetNamespace(), c)
	if err != nil {
		return nil, err
	}
	tlsClientConfig := rest.TLSClientConfig{}
	if _, err := certutil.NewPool(caPath); err != nil {
		klog.Errorf("expected to load root CA config from %s, but got err: %v", caPath, err)
	} else {
		tlsClientConfig.CAFile = caPath
	}
	apiHost, err := getAPIServerHost()
	if err != nil {
		return nil, err
	}
	return &rest.Config{
		Host:            apiHost,
		TLSClientConfig: tlsClientConfig,
		BearerToken:     token,
	}, nil
}

func ensureSpinSvc(spinSvc interfaces.SpinnakerService, c client.Client, ctx context.Context) (interfaces.SpinnakerService, error) {
	if spinSvc != nil {
		return spinSvc, nil
	}
	i := TypesFactory.NewServiceList()
	sc, err := secrets.FromContextWithError(ctx)
	if err != nil {
		return nil, err
	}
	list, err := util.GetSpinnakerServices(i, sc.Namespace, c)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	} else {
		// there should be only one spinnaker service per namespace
		return list[0], nil
	}
}

func getAPIServerHost() (string, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if len(host) == 0 || len(port) == 0 {
		// not running in cluster
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		cfg, err := rules.Load()
		if err != nil {
			return "", err
		}
		cc, err := clientcmd.NewDefaultClientConfig(*cfg, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return "", err
		}
		return cc.Host, nil
	}

	return fmt.Sprintf("https://%s", net.JoinHostPort(host, port)), nil
}

func makeOverrideFromAuthSettings(config *clientcmdapi.Config, settings authSettings) *clientcmd.ConfigOverrides {
	overrides := &clientcmd.ConfigOverrides{}
	if settings.Context != "" {
		overrides.CurrentContext = settings.Context
	}
	if settings.User != "" {
		if authInfo, ok := config.AuthInfos[settings.User]; ok {
			overrides.AuthInfo = *authInfo
		}
	}
	if settings.Cluster != "" {
		if cluster, ok := config.Clusters[settings.Cluster]; ok {
			overrides.ClusterInfo = *cluster
		}
	}
	if len(settings.OAuthScopes) > 0 {
		overrides.AuthInfo = clientcmdapi.AuthInfo{
			AuthProvider: &clientcmdapi.AuthProviderConfig{
				Name: "gcp",
				Config: map[string]string{
					"scopes": strings.Join(settings.OAuthScopes, ","),
				},
			},
		}
	}
	return overrides
}

type authSettings struct {
	// User to use in the kubeconfig file
	User string `json:"user,omitempty"`
	// Context to use in the kubeconfig file if not default
	Context string `json:"context,omitempty"`
	// Cluster to use in the kubeconfig file
	Cluster        string `json:"cluster,omitempty"`
	ServiceAccount bool   `json:"serviceAccount,omitempty"`
	// Reference to a kubeconfig file
	KubeconfigFile      string   `json:"kubeconfigFile,omitempty"`
	KubeconfigContents  string   `json:"kubeconfigContents,omitempty"`
	OAuthServiceAccount string   `json:"oAuthServiceAccount,omitempty"`
	OAuthScopes         []string `json:"oAuthScopes,omitempty"`
}

func (k *kubernetesAccountValidator) validateAccess(ctx context.Context, cc *rest.Config) error {
	clientset, err := kubernetes.NewForConfig(cc)
	if err != nil {
		return fmt.Errorf("unable to build kubernetes clientset from rest config: %w", err)
	}
	// We want to keep the validation short (ideally just one request), so any improvement should remain short (e.g. not a request per namespace)
	ns, err := inspect.GetStringArray(k.account.Settings, "namespaces")
	if err != nil || len(ns) == 0 {
		// If namespaces are not defined, a list namespaces call should be successful
		// The test is analogous to what is done in Halyard
		_, err = clientset.CoreV1().Namespaces().List(ctx, v13.ListOptions{})
		if err != nil {
			return fmt.Errorf("error listing namespaces in account \"%s\":\n  %w", k.account.Name, err)
		}
	} else {
		// Otherwise read resources just for the first namespace configured
		_, err = clientset.CoreV1().Pods(ns[0]).List(ctx, v13.ListOptions{})
		if err != nil {
			return fmt.Errorf("error listing pods in account \"%s\", namespace \"%s\":\n  %w", k.account.Name, ns[0], err)
		}
	}
	return nil
}

func (k *kubernetesAccountValidator) validateSettings(ctx context.Context, log logr.Logger) error {
	nss, err := inspect.GetStringArray(k.account.Settings, "namespaces")
	if err != nil {
		nss = make([]string, 0)
	}
	omitNss, err := inspect.GetStringArray(k.account.Settings, "omitNamespaces")
	if err != nil {
		omitNss = make([]string, 0)
	}
	if len(nss) > 0 && len(omitNss) > 0 {
		return fmt.Errorf("at most one of \"namespaces\" and \"omitNamespaces\" can be supplied.")
	}
	return nil
}
