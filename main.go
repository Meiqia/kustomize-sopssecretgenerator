// Copyright 2019 Go About B.V.
// Parts adapted from kustomize, Copyright 2019 The Kubernetes Authors.
// Licensed under the Apache License, Version 2.0.

package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/dimchansky/utfbom"
	"github.com/pkg/errors"
	"go.mozilla.org/sops"
	sopscommon "go.mozilla.org/sops/cmd/sops/common"
	sopsdecrypt "go.mozilla.org/sops/decrypt"
	"gopkg.in/yaml.v2"
)

const (
	apiVersion = "kustomize.meiqia.com/v1beta1"
	kind       = "SopsSecretGenerator"
)

type kvMap map[string]string

// TypeMeta defines the resource type
type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
}

// ObjectMeta contains Kubernetes resource metadata such as the name
type ObjectMeta struct {
	Name        string `json:"name" yaml:"name"`
	Namespace   string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	Labels      kvMap  `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations kvMap  `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

// SopsSecretGenerator is a generator for Secrets
type SopsSecretGenerator struct {
	TypeMeta              `json:",inline" yaml:",inline"`
	ObjectMeta            `json:"metadata" yaml:"metadata"`
	EnvSources            []string `json:"envs" yaml:"envs"`
	FileSources           []string `json:"files" yaml:"files"`
	Behavior              string   `json:"behavior,omitempty" yaml:"behavior,omitempty"`
	DisableNameSuffixHash bool     `json:"disableNameSuffixHash,omitempty" yaml:"disableNameSuffixHash,omitempty"`
	Type                  string   `json:"type,omitempty" yaml:"type,omitempty"`
}

// Secret is a Kubernetes Secret
type Secret struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Data       kvMap  `json:"data" yaml:"data"`
	Type       string `json:"type,omitempty" yaml:"type,omitempty"`
}

func main() {
	if len(os.Args) != 2 {
		_, _ = fmt.Fprintln(os.Stderr, "usage: SopsSecretGenerator FILE")
		os.Exit(1)
	}

	output, err := processSopsSecretGenerator(os.Args[1])
	if err != nil {
		if sopsErr, ok := errors.Cause(err).(sops.UserError); ok {
			_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n%s\n", err, sopsErr.UserError())
		} else {
			_, _ = fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		os.Exit(2)
	}
	fmt.Print(output)
}

func processSopsSecretGenerator(fn string) (string, error) {
	input, err := readInput(fn)
	if err != nil {
		return "", err
	}
	secret, err := generateSecret(input)
	if err != nil {
		return "", err
	}
	output, err := yaml.Marshal(secret)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

func generateSecret(sopsSecret SopsSecretGenerator) (Secret, error) {
	data, err := parseInput(sopsSecret)
	if err != nil {
		return Secret{}, err
	}

	annotations := make(kvMap)
	for k, v := range sopsSecret.Annotations {
		annotations[k] = v
	}
	if !sopsSecret.DisableNameSuffixHash {
		annotations["kustomize.config.k8s.io/needs-hash"] = "true"
	}
	if sopsSecret.Behavior != "" {
		annotations["kustomize.config.k8s.io/behavior"] = sopsSecret.Behavior
	}

	secret := Secret{
		TypeMeta: TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: ObjectMeta{
			Name:        sopsSecret.Name,
			Namespace:   sopsSecret.Namespace,
			Labels:      sopsSecret.Labels,
			Annotations: annotations,
		},
		Data: data,
		Type: sopsSecret.Type,
	}
	return secret, nil
}

func readInput(fn string) (SopsSecretGenerator, error) {
	content, err := ioutil.ReadFile(fn)
	if err != nil {
		return SopsSecretGenerator{}, err
	}

	input := SopsSecretGenerator{
		TypeMeta: TypeMeta{},
		ObjectMeta: ObjectMeta{
			Annotations: make(kvMap),
		},
	}
	err = yaml.Unmarshal(content, &input)
	if err != nil {
		return SopsSecretGenerator{}, err
	}

	if input.APIVersion != apiVersion || input.Kind != kind {
		return SopsSecretGenerator{}, errors.Errorf("input must be apiVersion %s, kind %s", apiVersion, kind)
	}
	if input.Name == "" {
		return SopsSecretGenerator{}, errors.New("input must contain metadata.name value")
	}
	return input, nil
}

func parseInput(input SopsSecretGenerator) (kvMap, error) {
	data := make(kvMap)
	err := parseEnvSources(input.EnvSources, data)
	if err != nil {
		return nil, err
	}
	err = parseFileSources(input.FileSources, data)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func parseEnvSources(sources []string, data kvMap) error {
	for _, source := range sources {
		err := parseEnvSource(source, data)
		if err != nil {
			return errors.Wrapf(err, "env source %v", source)
		}
	}
	return nil
}

func parseEnvSource(source string, data kvMap) error {
	content, err := ioutil.ReadFile(source)
	if err != nil {
		return err
	}

	format := formatForPath(source)
	decrypted, err := sopsdecrypt.Data(content, format)
	if err != nil {
		return err
	}

	switch format {
	case "dotenv":
		err = parseDotEnvContent(decrypted, data)
	case "yaml":
		err = parseYAMLContent(decrypted, data)
	case "json":
		err = parseJSONContent(decrypted, data)
	default:
		err = errors.New("unknown file format, use dotenv, yaml or json")
	}
	if err != nil {
		return err
	}

	return nil
}

func parseDotEnvContent(content []byte, data kvMap) error {
	scanner := bufio.NewScanner(utfbom.SkipOnly(bytes.NewReader(content)))
	lineNum := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		err := parseDotEnvLine(line, data)
		if err != nil {
			return errors.Wrapf(err, "line %d", lineNum)
		}
		lineNum++
	}
	return scanner.Err()
}

func parseDotEnvLine(line []byte, data kvMap) error {
	if !utf8.Valid(line) {
		return errors.New("invalid utf8 sequence")
	}

	line = bytes.TrimLeftFunc(line, unicode.IsSpace)

	if len(line) == 0 || line[0] == '#' {
		return nil
	}

	pair := strings.SplitN(string(line), "=", 2)
	if len(pair) != 2 {
		return fmt.Errorf("requires value: %v", string(line))
	}

	data[pair[0]] = base64.StdEncoding.EncodeToString([]byte(pair[1]))
	return nil
}

func parseYAMLContent(content []byte, data kvMap) error {
	d := make(kvMap)
	err := yaml.Unmarshal(content, &d)
	if err != nil {
		return err
	}
	for k, v := range d {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return nil
}

func parseJSONContent(content []byte, data kvMap) error {
	d := make(kvMap)
	err := json.Unmarshal(content, &d)
	if err != nil {
		return err
	}
	for k, v := range d {
		data[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return nil
}

func parseFileSources(sources []string, data kvMap) error {
	for _, source := range sources {
		err := parseFileSource(source, data)
		if err != nil {
			return errors.Wrapf(err, "file source %v", source)
		}
	}
	return nil
}

func parseFileSource(source string, data kvMap) error {
	key, fn, err := parseFileName(source)
	if err != nil {
		return err
	}

	content, err := ioutil.ReadFile(fn)
	if err != nil {
		return err
	}

	decrypted, err := sopsdecrypt.Data(content, formatForPath(source))
	if err != nil {
		return err
	}

	data[key] = base64.StdEncoding.EncodeToString(decrypted)
	return nil
}

func parseFileName(source string) (key string, fn string, err error) {
	components := strings.Split(source, "=")
	switch len(components) {
	case 1:
		return path.Base(source), source, nil
	case 2:
		key, fn = components[0], components[1]
		if key == "" {
			return "", "", fmt.Errorf("key name for file path %v missing", strings.TrimPrefix(source, "="))
		} else if fn == "" {
			return "", "", fmt.Errorf("file path for key name %v missing", strings.TrimSuffix(source, "="))
		}
		return key, fn, nil
	default:
		return "", "", errors.New("key names or file paths cannot contain '='")
	}
}

func formatForPath(path string) string {
	if sopscommon.IsYAMLFile(path) {
		return "yaml"
	} else if sopscommon.IsJSONFile(path) {
		return "json"
	} else if sopscommon.IsEnvFile(path) {
		return "dotenv"
	}
	return "binary"
}
