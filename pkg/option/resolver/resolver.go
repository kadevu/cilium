// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

// Package resolver provides the logic for merging in the various sources of configuration,
// overrides, and drop-ins.
package resolver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/sets"
	apivalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/cilium/cilium/pkg/annotation"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	ciliumv2alpha1 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2alpha1"
	"github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/time"
)

const (
	KindConfigMap  = "config-map"
	KindNode       = "node"
	KindNodeConfig = "cilium-node-config"

	ConfigSources          = "config-sources"
	ConfigSourcesOverrides = "config-sources-overrides"
)

type ConfigSource struct {
	Kind      string `json:"kind"`      // one of KindConfigMap, KindNodeConfig, KindNode
	Namespace string `json:"namespace"` // The namespace for the ConfigMap, CiliumNodeConfigs or empty for Node
	Name      string `json:"name"`      // The name of the ConfigMap or Node, unused for Overrides
}
type ConfigOverride struct {
	AllowConfigKeys []string `json:"allowConfigKeys"` // List of configuration keys that are allowed to be overridden (e.g. set from not the first source. Takes precedence over deny-config-keys
	DenyConfigKeys  []string `json:"denyConfigKeys"`  // List of configuration keys that are not allowed to be overridden (e.g. set from not the first source. If allow-config-keys is set, this field is ignored"
}

func (cs *ConfigSource) String() string {
	return fmt.Sprintf("%s:%s/%s", cs.Kind, cs.Namespace, cs.Name)
}

func ResolveConfigurations(ctx context.Context, logger *slog.Logger, client client.Clientset, nodeName string, sources []ConfigSource, allowConfigKeys, denyConfigKeys []string) (map[string]string, error) {
	config := map[string]string{}
	var sourceDescriptions []ConfigSource // We want to keep track of which unique sources we actually use in order of source priority

	// matchKeys is a set of keys that are either allowed or denied
	var matchKeys sets.Set[string]
	allowIfMatch := false // if true, then this is an allow list; otherwise a deny list
	if len(allowConfigKeys) > 0 {
		matchKeys = sets.New(allowConfigKeys...)
		allowIfMatch = true
	} else if len(denyConfigKeys) > 0 {
		matchKeys = sets.New(denyConfigKeys...)
	}

	first := true
	for _, source := range sources {
		c, descs, err := ReadConfigSource(ctx, logger, client, nodeName, source)
		if err != nil {
			return nil, fmt.Errorf("failed to read config source %s: %w", source.String(), err)
		}
		logger.Info(
			"Got configuration source",
			logfields.LenConfigPairs, len(c),
			logfields.ConfigSource, source,
		)
		if !first {
			for k := range c {
				if matchKeys != nil && !(matchKeys.Has(k) == allowIfMatch) {
					logger.Warn(
						"Source has non-overridable key",
						logfields.ConfigKey, k,
						logfields.ConfigSource, source,
					)
					delete(c, k)
				}
			}
		}
		first = false
		if len(c) != 0 {
			config = mergeConfig(logger, source, config, c)
			sourceDescriptions = append(sourceDescriptions, descs...)
		}
	}

	sConfigJson, err := json.Marshal(sourceDescriptions)
	if err != nil {
		return config, fmt.Errorf("encoding to JSON %s: %w", ConfigSources, err)
	}

	oConfigJson, err := json.Marshal(ConfigOverride{AllowConfigKeys: allowConfigKeys, DenyConfigKeys: denyConfigKeys})
	if err != nil {
		return config, fmt.Errorf("encoding to JSON %s: %w", ConfigSourcesOverrides, err)
	}

	config[ConfigSources] = string(sConfigJson)
	config[ConfigSourcesOverrides] = string(oConfigJson)

	return config, nil
}

func mergeConfig(logger *slog.Logger, source ConfigSource, lower, upper map[string]string) map[string]string {
	out := maps.Clone(lower)

	for k, v := range upper {
		if _, set := out[k]; set {
			logger.Info(
				"Source overrides key",
				logfields.ConfigKey, k,
				logfields.ConfigSource, source,
			)
		}
		out[k] = v
	}

	return out
}

// WriteConfigurations writes the key-value pairs in data to destDir. It writes it
// like a Kubernetes config-map: It uses a double-layer symlink to allow for
// atomic updates:
// destDir/key -> ..data/key
// ..data -> ..data_$time
func WriteConfigurations(ctx context.Context, logger *slog.Logger, destDir string, data map[string]string) error {
	dataDirName := fmt.Sprintf("..data_%d", time.Now().Unix())
	err := os.MkdirAll(filepath.Join(destDir, dataDirName), 0777)
	if err != nil {
		return fmt.Errorf("failed to create data directory %s", filepath.Join(destDir, dataDirName))
	}

	for k, v := range data {
		if strings.ContainsRune(k, os.PathSeparator) {
			logger.Error(
				"Ignoring key with path separator",
				logfields.ConfigKey, k,
			)
			continue
		}

		dest := filepath.Join(destDir, dataDirName, k)
		if err := os.WriteFile(dest, []byte(v), 0644); err != nil {
			return fmt.Errorf("failed to write config key at %s: %w", dest, err)
		}
	}

	_ = os.Remove(filepath.Join(destDir, "..data.tmp"))

	// can't atomically update symlinks, so create a new one and rename
	if err := os.Symlink(dataDirName, filepath.Join(destDir, "..data.tmp")); err != nil {
		return fmt.Errorf("failed to write ..data.tmp symlink: %w", err)
	}
	if err := os.Rename(filepath.Join(destDir, "..data.tmp"), filepath.Join(destDir, "..data")); err != nil {
		return fmt.Errorf("failed to move ..data symlink in to place: %w", err)
	}

	for k := range data {
		if err := os.Symlink(filepath.Join("..data", k), filepath.Join(destDir, k)); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to write key %s symlink: %w", k, err)
		}
	}

	return nil
}

func ReadConfigSource(ctx context.Context, logger *slog.Logger, client client.Clientset, nodeName string, source ConfigSource) (config map[string]string, sources []ConfigSource, err error) {
	logger.Info(
		"Reading configuration from source",
		logfields.ConfigSource, source,
	)
	switch source.Kind {
	case KindNode:
		return readNodeOverrides(ctx, logger, client, source.Name)
	case KindConfigMap:
		return readConfigMap(ctx, logger, client, source)
	case KindNodeConfig:
		return readNodeConfigsAllVersions(ctx, logger, client, nodeName, source.Namespace, source.Name)
	}
	return nil, nil, fmt.Errorf("invalid source kind %s", source.Kind)
}

func readNodeOverrides(ctx context.Context, logger *slog.Logger, client client.Clientset, nodeName string) (map[string]string, []ConfigSource, error) {
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("could not get Node %s: %w", nodeName, err)
	}

	// We allow overriding individual key-value pairs by annotating the Node object
	// with config.cilium.io/K=V
	out := map[string]string{}

	read := func(in map[string]string) {
		for k, v := range in {
			if strings.HasPrefix(k, annotation.ConfigPrefix) {
				s := strings.SplitN(k, "/", 2)
				if len(s) != 2 {
					logger.Error(
						fmt.Sprintf("Node annotation format invalid: should be of the format %s/<KEY>", annotation.ConfigPrefix),
						logfields.ConfigAnnotation, k,
					)
					continue
				}
				key := s[1]
				if errs := apivalidation.IsConfigMapKey(key); len(errs) > 0 {
					logger.Error(
						"Node annotation format invalid: invalid key",
						logfields.Errors, errs,
						logfields.ConfigKey, k,
					)
					continue
				}
				out[key] = v
			}
		}
	}

	read(node.Labels)
	read(node.Annotations)
	if len(out) == 0 {
		return nil, nil, nil
	}

	return out, []ConfigSource{{Kind: KindNode, Namespace: "", Name: nodeName}}, nil
}

func readConfigMap(ctx context.Context, logger *slog.Logger, client client.Clientset, source ConfigSource) (map[string]string, []ConfigSource, error) {
	cm, err := client.CoreV1().ConfigMaps(source.Namespace).Get(ctx, source.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.Error(
				"Configmap not found, ignoring",
				logfields.ConfigSource, source,
			)
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("failed to retrieve ConfigMap %s/%s: %w", source.Namespace, source.Name, err)
	}
	if len(cm.Data) == 0 {
		return nil, nil, nil
	}
	return cm.Data, []ConfigSource{source}, nil
}

// readNodeConfigsAllVersions read node configurations for versions v2 and v2alpha1 of CiliumNodeConfig CRD.
// TODO depreciate CNC on v2alpha1 https://github.com/cilium/cilium/issues/31982
func readNodeConfigsAllVersions(ctx context.Context, logger *slog.Logger, client client.Clientset, nodeName, namespace, name string) (map[string]string, []ConfigSource, error) {
	var errv2, errv2alpha1 error

	nodeConfigv2, descv2, errv2 := readNodeConfigs(ctx, logger, client, nodeName, namespace, name)
	if errv2 != nil {
		logger.Error(
			"CiliumNodeConfig v2 not found",
			logfields.Error, errv2,
			logfields.Node, nodeName,
		)
	}

	nodeConfigv2alpha1, descv2alpha1, errv2alpha1 := readNodeConfigsv2alpha1(ctx, logger, client, nodeName, namespace, name)
	if errv2alpha1 != nil {
		logger.Error(
			"CiliumNodeConfig v2alpha1 not found",
			logfields.Error, errv2alpha1,
			logfields.Node, nodeName,
		)
		// return the errors for the two versions
		if errv2 != nil {
			return nil, nil, fmt.Errorf("CiliumNodeConfig v2 and v2alpha1 not found: %w and %w\n", errv2, errv2alpha1)
		}
		return nil, nil, errv2alpha1
	}

	// Copiying values from a map into a nil map results in a panic, please refer to https://github.com/golang/go/issues/64390
	if nodeConfigv2alpha1 == nil {
		nodeConfigv2alpha1 = nodeConfigv2
	} else {
		// overwrite nodeConfigv2alpha1 with nodeConfigv2 values
		maps.Copy(nodeConfigv2alpha1, nodeConfigv2)
	}

	addedSources := sets.New[ConfigSource](descv2...)
	for _, source := range descv2alpha1 {
		if !addedSources.Has(source) {
			descv2 = append(descv2, source)
		}
		addedSources.Insert(source)
	}

	return nodeConfigv2alpha1, descv2, nil
}

// readNodeConfigs reads all the CiliumNodeConfig in v2 objects and returns a flattened map
// of any key overrides that apply to this node.
// TODO remove me when CiliumNodeConfig v2alpha1 is deprecated
func readNodeConfigs(ctx context.Context, logger *slog.Logger, client client.Clientset, nodeName, namespace, name string) (map[string]string, []ConfigSource, error) {
	var overrides []ciliumv2.CiliumNodeConfig

	// Retrieve CNCs if the name is not provided
	if name == "" {
		l, err := client.CiliumV2().CiliumNodeConfigs(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) { // Tolerate the CRD not existing
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("could not list CiliumNodeConfig objects: %w", err)
		}
		overrides = l.Items
	} else {
		// Retrieve CNCs with the given name
		o, err := client.CiliumV2().CiliumNodeConfigs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			if apierrors.IsNotFound(err) { // Tolerate the CRD not existing
				return nil, nil, nil
			}
			return nil, nil, fmt.Errorf("could not retrieve CiliumNodeConfig %s/%s: %w", namespace, name, err)
		} else if err == nil {
			overrides = append(overrides, *o)
		}
	}

	if len(overrides) == 0 {
		return nil, nil, nil
	}

	// If there are overrides, retrieve our node.
	// We'll need it to match label selectors
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("could not get Node %s: %w", nodeName, err)
	}

	matching := map[string]ciliumv2.CiliumNodeConfig{}

	// track names separately, since we will compute "priority" by lexicographic sort
	var matchingNames []string

	for _, override := range overrides {
		// ignore empty overrides
		if len(override.Spec.Defaults) == 0 {
			continue
		}

		// if we're selecting on a list, then evaluate the node selector
		if name == "" && override.Spec.NodeSelector != nil {
			ls, err := metav1.LabelSelectorAsSelector(override.Spec.NodeSelector)
			if err != nil { // unreachable
				return nil, nil, fmt.Errorf("invalid selector in CiliumNodeConfig %s: %w", override.Name, err)
			}
			if ls.Matches(labels.Set(node.Labels)) {
				matching[override.Name] = override
				matchingNames = append(matchingNames, override.Name)
			}
		} else if name != "" {
			matching[override.Name] = override
			matchingNames = append(matchingNames, override.Name)
		}
	}

	// Within overrides, lexicographical ordering determines priority.
	slices.Sort(matchingNames)

	out := make(map[string]string)
	for _, name := range matchingNames {
		for k, v := range matching[name].Spec.Defaults {
			if errs := apivalidation.IsConfigMapKey(k); len(errs) > 0 {
				logger.Error(
					"Invalid key in CiliumNodeConfigs",
					logfields.Name, name,
					logfields.K8sNamespace, matching[name].Namespace,
					logfields.ConfigKey, k,
				)
				continue
			}
			if _, set := out[k]; set {
				logger.Warn(
					"Key set in multiple CiliumNodeConfigs",
					logfields.ConfigKey, k,
				)
			}
			out[k] = v
		}
	}

	var sourceDescriptions []ConfigSource
	for _, name := range matchingNames {
		sourceDescriptions = append(sourceDescriptions, ConfigSource{Kind: KindNodeConfig, Namespace: namespace, Name: name})
	}

	return out, sourceDescriptions, nil
}

// readNodeConfigsv2alpha1 reads all the CiliumNodeConfig in v2alpha1 objects and returns a flattened map
// of any key overrides that apply to this node.
// TODO depreciate CNC on v2alpha1 https://github.com/cilium/cilium/issues/31982
func readNodeConfigsv2alpha1(ctx context.Context, logger *slog.Logger, client client.Clientset, nodeName, namespace, name string) (map[string]string, []ConfigSource, error) {
	var overrides []ciliumv2alpha1.CiliumNodeConfig

	// Retrieve CNCs if the name is not provided
	if name == "" {
		l, err := client.CiliumV2alpha1().CiliumNodeConfigs(namespace).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) { // Tolerate the CRD not existing
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("could not list CiliumNodeConfig v2alpha1 objects: %w", err)
		}
		overrides = l.Items
	} else {
		// Retrieve CNCs with the given name
		o, err := client.CiliumV2alpha1().CiliumNodeConfigs(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("could not retrieve CiliumNodeConfig v2alpha1 %s/%s: %w", namespace, name, err)
		}
		overrides = append(overrides, *o)
	}

	if len(overrides) == 0 {
		return nil, nil, nil
	}

	// If there are overrides, retrieve our node.
	// We'll need it to match label selectors
	node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, fmt.Errorf("could not get Node %s: %w", nodeName, err)
	}

	matching := map[string]ciliumv2alpha1.CiliumNodeConfig{}

	// track names separately, since we will compute "priority" by lexicographic sort
	var matchingNames []string

	for _, override := range overrides {
		// ignore empty overrides
		if len(override.Spec.Defaults) == 0 {
			continue
		}

		// if we're selecting on a list, then evaluate the node selector
		if name == "" && override.Spec.NodeSelector != nil {
			ls, err := metav1.LabelSelectorAsSelector(override.Spec.NodeSelector)
			if err != nil { // unreachable
				return nil, nil, fmt.Errorf("invalid selector in CiliumNodeConfig v2alpha1 %s: %w", override.Name, err)
			}
			if ls.Matches(labels.Set(node.Labels)) {
				matching[override.Name] = override
				matchingNames = append(matchingNames, override.Name)
			}
		} else if name != "" {
			matching[override.Name] = override
			matchingNames = append(matchingNames, override.Name)
		}
	}

	// Within overrides, lexicographical ordering determines priority.
	slices.Sort(matchingNames)

	out := make(map[string]string)
	for _, name := range matchingNames {
		for k, v := range matching[name].Spec.Defaults {
			if errs := apivalidation.IsConfigMapKey(k); len(errs) > 0 {
				logger.Error(
					"Invalid key in CiliumNodeConfigs v2alpha1",
					logfields.Name, name,
					logfields.K8sNamespace, matching[name].Namespace,
					logfields.ConfigKey, k,
				)
				continue
			}
			if _, set := out[k]; set {
				logger.Info(
					"Key set in multiple CiliumNodeConfigs v2alpha1",
					logfields.ConfigKey, k,
				)
			}
			out[k] = v
		}
	}

	var sourceDescriptions []ConfigSource
	for _, name := range matchingNames {
		sourceDescriptions = append(sourceDescriptions, ConfigSource{Kind: KindNodeConfig, Namespace: namespace, Name: name})
	}

	return out, sourceDescriptions, nil
}
