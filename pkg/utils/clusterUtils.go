package utils

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/argoproj/argo-cd/v2/common"
	appv1 "github.com/argoproj/argo-cd/v2/pkg/apis/application/v1alpha1"
	log "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
)

// The contents of this file are from
// github.com/argoproj/argo-cd/util/db/cluster.go
//
// The main difference is that ListClusters(...) calls the kubeclient directly,
// via `g.clientset.CoreV1().Secrets`, rather than using the `db.listClusterSecrets()``
// which appears to have a race condition on when it is called.
//
// I was reminded of this issue that I opened, which might be related:
// https://github.com/argoproj/argo-cd/issues/4755
//
// I hope to upstream this change in some form, so that we do not need to worry about
// Argo CD changing the logic on us.

var (
	localCluster = appv1.Cluster{
		Name:            "in-cluster",
		Server:          common.KubernetesInternalAPIServerAddr,
		ConnectionState: appv1.ConnectionState{Status: appv1.ConnectionStatusSuccessful},
	}
	initLocalCluster sync.Once
)

const (
	ArgoCDSecretTypeLabel   = "argocd.argoproj.io/secret-type"
	ArgoCDSecretTypeCluster = "cluster"
)

func ListClusters(ctx context.Context, clientset kubernetes.Interface, namespace string) (*appv1.ClusterList, error) {

	clusterSecretsList, err := clientset.CoreV1().Secrets(namespace).List(ctx,
		metav1.ListOptions{LabelSelector: common.LabelKeySecretType + "=" + common.LabelValueSecretTypeCluster})
	if err != nil {
		return nil, err
	}

	if clusterSecretsList == nil {
		return nil, nil
	}

	clusterSecrets := clusterSecretsList.Items

	clusterList := appv1.ClusterList{
		Items: make([]appv1.Cluster, len(clusterSecrets)),
	}
	hasInClusterCredentials := false
	for i, clusterSecret := range clusterSecrets {
		cluster := *secretToCluster(&clusterSecret)
		clusterList.Items[i] = cluster
		if cluster.Server == common.KubernetesInternalAPIServerAddr {
			hasInClusterCredentials = true
		}
	}
	if !hasInClusterCredentials {
		localCluster := getLocalCluster(clientset)
		if localCluster != nil {
			clusterList.Items = append(clusterList.Items, *localCluster)
		}
	}
	return &clusterList, nil
}

func getLocalCluster(clientset kubernetes.Interface) *appv1.Cluster {
	initLocalCluster.Do(func() {
		info, err := clientset.Discovery().ServerVersion()
		if err == nil {
			localCluster.ServerVersion = fmt.Sprintf("%s.%s", info.Major, info.Minor)
			localCluster.ConnectionState = appv1.ConnectionState{Status: appv1.ConnectionStatusSuccessful}
		} else {
			localCluster.ConnectionState = appv1.ConnectionState{
				Status:  appv1.ConnectionStatusFailed,
				Message: err.Error(),
			}
		}
	})
	cluster := localCluster.DeepCopy()
	now := metav1.Now()
	cluster.ConnectionState.ModifiedAt = &now
	return cluster
}

// secretToCluster converts a secret into a Cluster object
func secretToCluster(s *corev1.Secret) *appv1.Cluster {
	var config appv1.ClusterConfig
	if len(s.Data["config"]) > 0 {
		err := json.Unmarshal(s.Data["config"], &config)
		if err != nil {
			panic(err)
		}
	}

	var namespaces []string
	for _, ns := range strings.Split(string(s.Data["namespaces"]), ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			namespaces = append(namespaces, ns)
		}
	}
	var refreshRequestedAt *metav1.Time
	if v, found := s.Annotations[common.AnnotationKeyRefresh]; found {
		requestedAt, err := time.Parse(time.RFC3339, v)
		if err != nil {
			log.Warnf("Error while parsing date in cluster secret '%s': %v", s.Name, err)
		} else {
			refreshRequestedAt = &metav1.Time{Time: requestedAt}
		}
	}
	var shard *int64
	if shardStr := s.Data["shard"]; shardStr != nil {
		if val, err := strconv.Atoi(string(shardStr)); err != nil {
			log.Warnf("Error while parsing shard in cluster secret '%s': %v", s.Name, err)
		} else {
			shard = pointer.Int64Ptr(int64(val))
		}
	}
	cluster := appv1.Cluster{
		ID:                 string(s.UID),
		Server:             strings.TrimRight(string(s.Data["server"]), "/"),
		Name:               string(s.Data["name"]),
		Namespaces:         namespaces,
		Config:             config,
		RefreshRequestedAt: refreshRequestedAt,
		Shard:              shard,
	}
	return &cluster
}

// ValidateDestination checks:
// if we used destination name we infer the server url
// if we used both name and server then we return an invalid spec error
func ValidateDestination(ctx context.Context, dest *appv1.ApplicationDestination, clientset kubernetes.Interface, namespace string) error {
	if dest.Name != "" {
		if dest.Server == "" {
			server, err := getDestinationServer(ctx, dest.Name, clientset, namespace)
			if err != nil {
				return fmt.Errorf("unable to find destination server: %v", err)
			}
			if server == "" {
				return fmt.Errorf("application references destination cluster %s which does not exist", dest.Name)
			}
			dest.SetInferredServer(server)
		} else {
			if !dest.IsServerInferred() {
				return fmt.Errorf("application destination can't have both name and server defined: %s %s", dest.Name, dest.Server)
			}
		}
	}
	return nil
}

func getDestinationServer(ctx context.Context, clusterName string, clientset kubernetes.Interface, namespace string) (string, error) {

	clusterList, err := ListClusters(ctx, clientset, namespace)
	if err != nil {
		return "", err
	}
	var servers []string
	for _, c := range clusterList.Items {
		if c.Name == clusterName {
			servers = append(servers, c.Server)
		}
	}
	if len(servers) > 1 {
		return "", fmt.Errorf("there are %d clusters with the same name: %v", len(servers), servers)
	} else if len(servers) == 0 {
		return "", fmt.Errorf("there are no clusters with this name: %s", clusterName)
	}
	return servers[0], nil
}
