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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/googleapis/librarian/internal/command"
	"github.com/googleapis/librarian/internal/config"
	"github.com/googleapis/librarian/internal/filesystem"
	"github.com/googleapis/librarian/internal/proto"
	"github.com/googleapis/librarian/internal/semver"
	"github.com/googleapis/librarian/internal/sources"
	"github.com/googleapis/librarian/internal/tool/protoc"
	"github.com/googleapis/librarian/internal/yaml"
)

const (
	defaultGroupID          = "com.google.cloud.kotlin"
	defaultArtifactIDPrefix = "google-cloud-kotlin-"
	commonResourcesProto    = "google/cloud/common_resources.proto"
)

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
	IncludeDirs          []string `json:"includeDirs"`
	ProtoFiles           []string `json:"protoFiles"`
	AdditionalProtoFiles []string `json:"additionalProtoFiles"`
}

// GenerateLibraries generates Kotlin client libraries using the repository's :generator tool.
func GenerateLibraries(ctx context.Context, cfg *config.Config, libraries []*config.Library, srcs *sources.Sources) error {
	var pc *config.Protoc
	if cfg.Tools != nil && cfg.Tools.Protoc != nil {
		pc = cfg.Tools.Protoc
		if err := protoc.Install(ctx, pc); err != nil {
			return fmt.Errorf("failed to install protoc: %w", err)
		}
	}
	protocPath, err := protoc.BinaryPathOrSystem(pc)
	if err != nil {
		return fmt.Errorf("failed to resolve protoc binary: %w", err)
	}

	generatorBin, err := ensureGeneratorInstalled(ctx)
	if err != nil {
		return fmt.Errorf("failed to build generator: %w", err)
	}

	var entries []batchEntry
	for _, library := range libraries {
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

		protoSrcDir := filepath.Join(outdir, "src", "main", "proto")
		protoFiles := []string{}
		additionalProtoFiles := []string{}
		additionalSeen := make(map[string]bool)

		for _, api := range library.APIs {
			if api.Java != nil && api.Java.GenerateGAPIC != nil && !*api.Java.GenerateGAPIC {
				continue
			}
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

		entries = append(entries, batchEntry{
			Name:                 library.Name,
			OutputDir:            filepath.Join(outdir, "src", "main", "kotlin"),
			JavaOutputDir:        filepath.Join(outdir, "src", "main", "java"),
			IncludeDirs:          includeDirs,
			ProtoFiles:           protoFiles,
			AdditionalProtoFiles: additionalProtoFiles,
		})
	}

	if len(entries) == 0 {
		return nil
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

	args := []string{
		"--protoc=" + protocPath,
		"--batch_spec=" + tempFile.Name(),
	}
	return command.RunStreaming(ctx, generatorBin, args...)
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
