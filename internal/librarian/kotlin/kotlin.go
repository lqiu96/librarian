// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package kotlin provides Kotlin specific functionality for librarian.
package kotlin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/googleapis/librarian/internal/cache"
	"github.com/googleapis/librarian/internal/command"
	"github.com/googleapis/librarian/internal/config"
	"github.com/googleapis/librarian/internal/filesystem"
	"github.com/googleapis/librarian/internal/proto"
	"github.com/googleapis/librarian/internal/semver"
	"github.com/googleapis/librarian/internal/sources"
	"github.com/googleapis/librarian/internal/tool/maven"
	"github.com/googleapis/librarian/internal/tool/protoc"
	"github.com/googleapis/librarian/internal/yaml"
	goproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
)

const (
	defaultGroupID          = "com.google.cloud.kotlin"
	defaultArtifactIDPrefix = "google-cloud-kotlin-"
	commonResourcesProto    = "google/cloud/common_resources.proto"

	// grpcPluginTool is the tools.maven entry name that provides the gRPC-Java
	// protoc plugin.
	grpcPluginTool = "protoc-gen-grpc-java"
	kotlinToolsDir = "kotlin_tools"
)

// commonProtoDirs lists standard protobuf directories whose compiled Java
// classes are already provided on every client module's classpath by
// protobuf-java, :clients:common-protos, :clients:common-iam, or :clients:common-grpc.
var commonProtoDirs = map[string]bool{
	"google/protobuf":          true,
	"google/protobuf/compiler": true,
	"google/api":               true,
	"google/rpc":               true,
	"google/rpc/context":       true,
	"google/type":              true,
	"google/geo/type":          true,
	"google/logging/type":      true,
	"google/shopping/type":     true,
	"google/cloud/audit":       true,
	"google/apps/card/v1":      true,
	"google/iam/v1":            true,
	"google/iam/v2":            true,
	"google/iam/v2beta":        true,
	"google/iam/v3":            true,
	"google/iam/v3beta":        true,
	"google/longrunning":       true,
	"google/cloud/location":    true,
}

// errUnsupportedPlatform indicates the host has no published gRPC-Java plugin build.
var errUnsupportedPlatform = errors.New("unsupported platform for protoc-gen-grpc-java")

// DefaultOutput derives the default output directory name for a Kotlin library.
func DefaultOutput(name, defaultOut string) string {
	if defaultOut == "" {
		defaultOut = "clients"
	}
	return path.Join(defaultOut, name)
}

// Fill populates Kotlin-specific default values for the library.
func Fill(library *config.Library) (*config.Library, error) {
	if library.Java == nil {
		library.Java = &config.JavaModule{}
	}
	if library.Java.GroupID == "" {
		library.Java.GroupID = defaultGroupID
	}
	if library.Java.ArtifactID == "" {
		library.Java.ArtifactID = defaultArtifactIDPrefix + library.Name
	}
	for _, api := range library.APIs {
		if api.Java == nil {
			api.Java = &config.JavaAPI{}
		}
		if api.Java.GenerateGAPIC == nil {
			api.Java.GenerateGAPIC = new(true)
		}
	}
	return library, nil
}

// Tidy tidies the Kotlin-specific configuration for a library by removing default values.
func Tidy(library *config.Library) (*config.Library, error) {
	if library.Java != nil {
		if library.Java.GroupID == defaultGroupID {
			library.Java.GroupID = ""
		}
		if library.Java.ArtifactID == defaultArtifactIDPrefix+library.Name {
			library.Java.ArtifactID = ""
		}
		var err error
		if library.Java, err = yaml.ClearIfEmpty(library.Java); err != nil {
			return nil, err
		}
	}
	for _, api := range library.APIs {
		if api.Java == nil {
			continue
		}
		if api.Java.GenerateGAPIC != nil && *api.Java.GenerateGAPIC {
			api.Java.GenerateGAPIC = nil
		}
		api.Java.AdditionalProtos = slices.DeleteFunc(api.Java.AdditionalProtos, func(p *config.AdditionalProto) bool {
			return p == nil || p.Path == ""
		})
		var err error
		if api.Java, err = yaml.ClearIfEmpty(api.Java); err != nil {
			return nil, err
		}
	}
	return library, nil
}

var kotlinSkipDuplicatePaths = map[string]bool{
	"google/iam/v1":     true,
	"google/iam/v2":     true,
	"google/iam/v2beta": true,
	"google/iam/v3":     true,
	"google/iam/v3beta": true,
}

// Validate checks that the Kotlin configuration is valid.
func Validate(cfg *config.Config) error {
	var errs []error
	pathCount := make(map[string]int)
	for _, library := range cfg.Libraries {
		if library.Version != "" {
			if _, err := semver.Parse(library.Version); err != nil {
				errs = append(errs, fmt.Errorf("library %q: invalid version %q: %w", library.Name, library.Version, err))
			}
		}
		for _, api := range library.APIs {
			if api.Path != "" {
				pathCount[api.Path]++
			}
		}
	}
	for p, count := range pathCount {
		if count > 1 && !kotlinSkipDuplicatePaths[p] {
			errs = append(errs, fmt.Errorf("duplicate api path: %s (appears %d times)", p, count))
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// Clean removes generated Kotlin/Java/proto files and build.gradle.kts in the library's output directory,
// while preserving src/test and any paths in library.Keep.
func Clean(library *config.Library) error {
	outDir := library.Output
	if outDir == "" {
		return nil
	}
	keepSet := make(map[string]bool)
	for _, k := range library.Keep {
		keepSet[filepath.ToSlash(k)] = true
	}

	for _, relDir := range []string{"src/main/kotlin", "src/main/java", "src/main/proto"} {
		dirPath := filepath.Join(outDir, filepath.FromSlash(relDir))
		if _, err := os.Stat(dirPath); err == nil {
			if !keepSet[relDir] {
				if err := os.RemoveAll(dirPath); err != nil {
					return fmt.Errorf("failed to remove %s: %w", dirPath, err)
				}
			}
		}
	}

	gradleFile := filepath.Join(outDir, "build.gradle.kts")
	if _, err := os.Stat(gradleFile); err == nil {
		if !keepSet["build.gradle.kts"] {
			if err := os.Remove(gradleFile); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("failed to remove %s: %w", gradleFile, err)
			}
		}
	}
	return nil
}

type batchEntry struct {
	Name                 string   `json:"name"`
	OutputDir            string   `json:"outputDir"`
	JavaOutputDir        string   `json:"javaOutputDir"`
	DescriptorSetFile    string   `json:"descriptorSetFile"`
	IncludeDirs          []string `json:"includeDirs"`
	ProtoFiles           []string `json:"protoFiles"`
	AdditionalProtoFiles []string `json:"additionalProtoFiles"`
	ServiceYamlFiles     []string `json:"serviceYamlFiles"`
}

// findServiceYamls returns the google.api.Service configuration files in dir. These carry the
// mixin API list and the per-service http rule overrides that the protos themselves do not declare.
func findServiceYamls(dir string) []string {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var found []string
	for _, e := range dirEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if bytes.Contains(data, []byte("type: google.api.Service")) {
			found = append(found, p)
		}
	}
	return found
}

// GenerateLibraries compiles protobuf descriptors and Java/gRPC wire classes in parallel via protoc,
// then invokes the repository's :generator tool in a single JVM batch run.
func GenerateLibraries(ctx context.Context, cfg *config.Config, libraries []*config.Library, srcs *sources.Sources) error {
	protocPath := "protoc"
	var protocIncludeDir string
	if cfg.Tools != nil && cfg.Tools.Protoc != nil {
		pc := cfg.Tools.Protoc
		if err := protoc.Install(ctx, pc); err != nil {
			return fmt.Errorf("failed to install protoc: %w", err)
		}
		var err error
		protocPath, err = protoc.BinaryPathOrSystem(pc)
		if err != nil {
			return fmt.Errorf("failed to resolve protoc binary: %w", err)
		}
		if pc.Version != "" {
			if installDir, dirErr := protoc.InstallDir(pc.Version); dirErr == nil {
				inc := filepath.Join(installDir, "include")
				if st, statErr := os.Stat(inc); statErr == nil && st.IsDir() {
					protocIncludeDir = inc
				}
			}
		}
	}

	grpcPluginPath, err := installGRPCPlugin(ctx, cfg.Tools)
	if err != nil {
		return err
	}

	generatorBin, err := ensureGeneratorInstalled(ctx)
	if err != nil {
		return fmt.Errorf("failed to build generator: %w", err)
	}

	tempDescRoot, err := os.MkdirTemp("", "librarian-kotlin-desc-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary descriptor directory: %w", err)
	}
	defer os.RemoveAll(tempDescRoot)

	var entries []batchEntry
	for idx, library := range libraries {
		outdir, err := filepath.Abs(library.Output)
		if err != nil {
			return fmt.Errorf("failed to resolve output directory for %s: %w", library.Name, err)
		}
		if err := os.MkdirAll(outdir, 0o755); err != nil {
			return fmt.Errorf("failed to create output directory %q: %w", outdir, err)
		}

		srcCfg := sources.NewSourceConfig(srcs, library.Roots)
		if len(srcCfg.ActiveRoots) == 0 {
			continue
		}
		primaryDir := srcCfg.Root(srcCfg.ActiveRoots[0])
		googleapisDir := srcCfg.Root("googleapis")

		includeDirs := []string{}
		for _, root := range srcCfg.ActiveRoots {
			includeDirs = append(includeDirs, srcCfg.Root(root))
		}
		if protocIncludeDir != "" {
			includeDirs = append(includeDirs, protocIncludeDir)
		}

		protoSrcDir := filepath.Join(outdir, "src", "main", "proto")
		protoFiles := []string{}
		additionalProtoFiles := []string{}
		additionalSeen := make(map[string]bool)
		serviceYamlFiles := []string{}
		serviceYamlSeen := make(map[string]bool)

		for _, api := range library.APIs {
			apiDir := filepath.Join(primaryDir, api.Path)
			apiProtos, err := proto.Gather(apiDir, api.Path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("failed to gather protos for %s (%s): %w", library.Name, api.Path, err)
			}
			if api.Java != nil && len(api.Java.ExcludedProtos) > 0 {
				apiProtos = filterProtos(apiProtos, api.Java.ExcludedProtos, primaryDir)
			}
			protoFiles = append(protoFiles, apiProtos...)

			for _, y := range findServiceYamls(apiDir) {
				if !serviceYamlSeen[y] {
					serviceYamlSeen[y] = true
					serviceYamlFiles = append(serviceYamlFiles, y)
				}
			}

			for _, p := range apiProtos {
				if rel, relErr := filepath.Rel(primaryDir, p); relErr == nil {
					destProto := filepath.Join(protoSrcDir, rel)
					if mkErr := os.MkdirAll(filepath.Dir(destProto), 0o755); mkErr == nil {
						_ = filesystem.CopyFile(p, destProto)
					}
				}
			}

			omitCommon := api.Java != nil && api.Java.OmitCommonResources
			if !omitCommon && googleapisDir != "" {
				commonPath := filepath.Join(googleapisDir, filepath.FromSlash(commonResourcesProto))
				if !additionalSeen[commonPath] {
					if _, statErr := os.Stat(commonPath); statErr == nil {
						additionalSeen[commonPath] = true
						additionalProtoFiles = append(additionalProtoFiles, commonPath)
					}
				}
			}
			if api.Java != nil {
				for _, addProto := range api.Java.AdditionalProtos {
					if addProto == nil || addProto.Path == "" {
						continue
					}
					addPath := filepath.Join(googleapisDir, filepath.FromSlash(addProto.Path))
					if _, statErr := os.Stat(addPath); statErr != nil {
						continue
					}
					if addProto.GenerateProtoClasses || addProto.CopyToOutput {
						if rel, relErr := filepath.Rel(googleapisDir, addPath); relErr == nil {
							destProto := filepath.Join(protoSrcDir, rel)
							if mkErr := os.MkdirAll(filepath.Dir(destProto), 0o755); mkErr == nil {
								_ = filesystem.CopyFile(addPath, destProto)
							}
						}
					}
					if addProto.GenerateProtoClasses {
						protoFiles = append(protoFiles, addPath)
					} else if !additionalSeen[addPath] {
						additionalSeen[addPath] = true
						additionalProtoFiles = append(additionalProtoFiles, addPath)
					}
				}
			}
		}

		if len(protoFiles) == 0 {
			continue
		}

		descFile := filepath.Join(tempDescRoot, fmt.Sprintf("%d_%s_descriptor_set.pb", idx, library.Name))
		entries = append(entries, batchEntry{
			Name:                 library.Name,
			OutputDir:            filepath.Join(outdir, "src", "main", "kotlin"),
			JavaOutputDir:        filepath.Join(outdir, "src", "main", "java"),
			DescriptorSetFile:    descFile,
			IncludeDirs:          includeDirs,
			ProtoFiles:           protoFiles,
			AdditionalProtoFiles: additionalProtoFiles,
			ServiceYamlFiles:     serviceYamlFiles,
		})
	}

	if len(entries) == 0 {
		return nil
	}

	if err := runBatchProtoc(ctx, entries, protocPath, grpcPluginPath); err != nil {
		return err
	}

	tempFile, err := os.CreateTemp("", "librarian-kotlin-batch-*.json")
	if err != nil {
		return fmt.Errorf("failed to create temporary batch spec file: %w", err)
	}
	defer os.Remove(tempFile.Name())

	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize batch spec: %w", err)
	}
	if err := os.WriteFile(tempFile.Name(), data, 0o644); err != nil {
		return fmt.Errorf("failed to write batch spec file: %w", err)
	}

	return command.RunStreaming(ctx, generatorBin, "--batch_spec="+tempFile.Name())
}

// runBatchProtoc executes protoc across all batch entries using a bounded worker pool.
func runBatchProtoc(ctx context.Context, entries []batchEntry, protocPath, grpcPluginPath string) error {
	workers := max(runtime.NumCPU(), 1)
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	errs := make([]error, len(entries))

	for i, entry := range entries {
		wg.Add(1)
		go func(idx int, e batchEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := compileEntryProtos(ctx, e, protocPath, grpcPluginPath); err != nil {
				errs[idx] = fmt.Errorf("protoc failed for library %s: %w", e.Name, err)
			}
		}(i, entry)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// compileEntryProtos compiles the Java/gRPC wire classes into e.JavaOutputDir (strictly for e.ProtoFiles
// plus any non-common transitive imports) and the full FileDescriptorSet into e.DescriptorSetFile
// (including e.AdditionalProtoFiles such as mixin protos and common_resources.proto).
func compileEntryProtos(ctx context.Context, e batchEntry, protocPath, grpcPluginPath string) error {
	distinctProtos := uniqueCanonicalPaths(e.ProtoFiles)
	allDescProtos := uniqueCanonicalPaths(append(append([]string{}, distinctProtos...), e.AdditionalProtoFiles...))
	includeDirs := uniqueCanonicalPaths(e.IncludeDirs)

	buildBaseArgs := func() []string {
		var args []string
		for _, inc := range includeDirs {
			if st, err := os.Stat(inc); err == nil && st.IsDir() {
				args = append(args, "-I", inc)
			}
		}
		args = append(args, "--experimental_allow_proto3_optional")
		return args
	}

	addWireArgs := func(args []string, outDir string) []string {
		if grpcPluginPath != "" {
			if _, err := os.Stat(grpcPluginPath); err == nil {
				args = append(args,
					"--plugin=protoc-gen-grpc-java="+grpcPluginPath,
					"--grpc-java_out="+outDir,
				)
			}
		}
		return append(args, "--java_out="+outDir)
	}

	addDescArgs := func(args []string, descOut string) []string {
		return append(args,
			"--descriptor_set_out="+descOut,
			"--include_imports",
			"--include_source_info",
		)
	}

	runCmd := func(args []string) error {
		cmd := exec.CommandContext(ctx, protocPath, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(e.DescriptorSetFile), 0o755); err != nil {
		return err
	}

	if e.JavaOutputDir != "" && len(distinctProtos) > 0 {
		if err := os.MkdirAll(e.JavaOutputDir, 0o755); err != nil {
			return err
		}
		wireArgs := addWireArgs(buildBaseArgs(), e.JavaOutputDir)
		if len(e.AdditionalProtoFiles) == 0 {
			wireArgs = addDescArgs(wireArgs, e.DescriptorSetFile)
			wireArgs = append(wireArgs, distinctProtos...)
			if err := runCmd(wireArgs); err != nil {
				return err
			}
			return compileExtraNonCommonImports(e.DescriptorSetFile, distinctProtos, includeDirs, e.JavaOutputDir, buildBaseArgs, runCmd)
		}
		wireArgs = append(wireArgs, distinctProtos...)
		if err := runCmd(wireArgs); err != nil {
			return err
		}
	}

	descArgs := addDescArgs(buildBaseArgs(), e.DescriptorSetFile)
	descArgs = append(descArgs, allDescProtos...)
	if err := runCmd(descArgs); err != nil {
		return err
	}
	if e.JavaOutputDir != "" && len(distinctProtos) > 0 {
		return compileExtraNonCommonImports(e.DescriptorSetFile, distinctProtos, includeDirs, e.JavaOutputDir, buildBaseArgs, runCmd)
	}
	return nil
}

func compileExtraNonCommonImports(
	descFile string,
	ownProtos []string,
	includeDirs []string,
	javaOutDir string,
	buildBaseArgs func() []string,
	runCmd func([]string) error,
) error {
	extra := resolveNonCommonImportedProtos(descFile, ownProtos, includeDirs)
	if len(extra) == 0 {
		return nil
	}
	args := append(buildBaseArgs(), "--java_out="+javaOutDir)
	args = append(args, extra...)
	return runCmd(args)
}

// resolveNonCommonImportedProtos inspects descFile and returns absolute paths for any transitive
// .proto imports of ownProtos that are not in ownProtos or commonProtoDirs.
func resolveNonCommonImportedProtos(descFile string, ownProtos []string, includeDirs []string) []string {
	raw, err := os.ReadFile(descFile)
	if err != nil {
		return nil
	}
	var descSet descriptorpb.FileDescriptorSet
	if err := goproto.Unmarshal(raw, &descSet); err != nil {
		return nil
	}

	normOwnPaths := make([]string, 0, len(ownProtos))
	for _, p := range ownProtos {
		if abs, err := filepath.EvalSymlinks(p); err == nil {
			normOwnPaths = append(normOwnPaths, filepath.ToSlash(abs))
		} else {
			normOwnPaths = append(normOwnPaths, filepath.ToSlash(p))
		}
	}

	ownNames := make(map[string]bool)
	fileByName := make(map[string]*descriptorpb.FileDescriptorProto, len(descSet.File))
	for _, fp := range descSet.File {
		name := fp.GetName()
		fileByName[name] = fp
		normName := strings.TrimPrefix(filepath.ToSlash(name), "/")
		for _, own := range normOwnPaths {
			if own == normName || strings.HasSuffix(own, "/"+normName) {
				ownNames[name] = true
				break
			}
		}
	}

	visited := make(map[string]bool)
	var queue []string
	for own := range ownNames {
		visited[own] = true
		queue = append(queue, own)
	}
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		fp := fileByName[curr]
		if fp == nil {
			continue
		}
		for _, dep := range fp.GetDependency() {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
			}
		}
	}

	var resolved []string
	for dep := range visited {
		if ownNames[dep] {
			continue
		}
		norm := strings.TrimPrefix(filepath.ToSlash(dep), "/")
		if norm == commonResourcesProto || commonProtoDirs[path.Dir(norm)] {
			continue
		}
		for _, inc := range includeDirs {
			candidate := filepath.Join(inc, filepath.FromSlash(norm))
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				resolved = append(resolved, candidate)
				break
			}
		}
	}
	return uniqueCanonicalPaths(resolved)
}

func uniqueCanonicalPaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	var out []string
	for _, p := range paths {
		key := p
		if abs, err := filepath.EvalSymlinks(p); err == nil {
			key = abs
		} else if abs, err := filepath.Abs(p); err == nil {
			key = abs
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, p)
		}
	}
	return out
}

// installGRPCPlugin installs the gRPC-Java protoc plugin declared under
// tools.maven and returns the path to its executable wrapper. It returns an
// empty path when the plugin is not configured, in which case the generator
// falls back to its own plugin discovery.
func installGRPCPlugin(ctx context.Context, tools *config.Tools) (string, error) {
	if tools == nil {
		return "", nil
	}
	var tool *config.MavenTool
	for _, t := range tools.Maven {
		if t != nil && t.Name == grpcPluginTool {
			tool = t
			break
		}
	}
	if tool == nil {
		return "", nil
	}
	// The plugin is published as a per-platform executable. Deriving the
	// classifier here keeps librarian.yaml portable across developer machines.
	resolved := *tool
	if resolved.Classifier == "" {
		classifier, err := grpcPluginClassifier()
		if err != nil {
			return "", err
		}
		resolved.Classifier = classifier
	}

	binDir, libDir, err := kotlinToolDirs()
	if err != nil {
		return "", err
	}
	for _, dir := range []string{binDir, libDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("failed to create tool directory %q: %w", dir, err)
		}
	}
	if err := maven.Install(ctx, []*config.MavenTool{&resolved}, binDir, libDir); err != nil {
		return "", fmt.Errorf("failed to install %s: %w", grpcPluginTool, err)
	}
	return filepath.Join(binDir, resolved.Name), nil
}

// grpcPluginClassifier maps the current platform onto the Maven classifier used
// by io.grpc:protoc-gen-grpc-java releases.
func grpcPluginClassifier() (string, error) {
	var goos string
	switch runtime.GOOS {
	case "darwin":
		goos = "osx"
	case "linux":
		goos = "linux"
	case "windows":
		goos = "windows"
	default:
		return "", fmt.Errorf("%w: %s", errUnsupportedPlatform, runtime.GOOS)
	}
	var goarch string
	switch runtime.GOARCH {
	case "amd64":
		goarch = "x86_64"
	case "arm64":
		goarch = "aarch_64"
	default:
		return "", fmt.Errorf("%w: %s", errUnsupportedPlatform, runtime.GOARCH)
	}
	return goos + "-" + goarch, nil
}

// kotlinToolDirs returns the bin and lib directories for Kotlin tool installs.
func kotlinToolDirs() (string, string, error) {
	base, err := cache.BinDirectory()
	if err != nil {
		return "", "", err
	}
	installDir, err := filepath.Abs(filepath.Join(base, kotlinToolsDir))
	if err != nil {
		return "", "", err
	}
	return filepath.Join(installDir, "bin"), filepath.Join(installDir, "lib"), nil
}

func ensureGeneratorInstalled(ctx context.Context) (string, error) {
	binPath, err := filepath.Abs(filepath.Join("generator", "build", "install", "generator", "bin", "generator"))
	if err != nil {
		return "", err
	}
	gradlew, err := filepath.Abs("gradlew")
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(gradlew); err == nil {
		if err := command.RunStreaming(ctx, gradlew, ":generator:installDist", "-q"); err != nil {
			return "", fmt.Errorf("gradle installDist failed: %w", err)
		}
	}
	if _, err := os.Stat(binPath); err != nil {
		return "", fmt.Errorf("generator binary not found at %s: %w", binPath, err)
	}
	return binPath, nil
}

func filterProtos(fullPaths []string, relExcludes []string, root string) []string {
	if len(relExcludes) == 0 {
		return fullPaths
	}
	excludedSet := make(map[string]bool, len(relExcludes))
	for _, e := range relExcludes {
		fullPath := filepath.ToSlash(filepath.Join(root, filepath.FromSlash(e)))
		excludedSet[fullPath] = true
	}
	filtered := make([]string, 0, len(fullPaths))
	for _, p := range fullPaths {
		if excludedSet[filepath.ToSlash(p)] {
			continue
		}
		filtered = append(filtered, p)
	}
	return filtered
}
