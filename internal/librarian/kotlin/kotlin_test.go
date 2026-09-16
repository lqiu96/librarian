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

package kotlin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/googleapis/librarian/internal/config"
)

func TestDefaultOutput(t *testing.T) {
	if got, want := DefaultOutput("kms", ""), "clients/kms"; got != want {
		t.Errorf("DefaultOutput(\"kms\", \"\") = %q, want %q", got, want)
	}
	if got, want := DefaultOutput("kms", "custom"), "custom/kms"; got != want {
		t.Errorf("DefaultOutput(\"kms\", \"custom\") = %q, want %q", got, want)
	}
}

func TestFillAndTidy(t *testing.T) {
	lib := &config.Library{
		Name: "secretmanager",
		APIs: []*config.API{
			{Path: "google/cloud/secretmanager/v1"},
		},
	}

	filled, err := Fill(lib)
	if err != nil {
		t.Fatalf("Fill() unexpected error: %v", err)
	}
	if filled.Java == nil {
		t.Fatal("Fill() expected library.Java to be populated")
	}
	if got, want := filled.Java.GroupID, defaultGroupID; got != want {
		t.Errorf("filled.Java.GroupID = %q, want %q", got, want)
	}
	if got, want := filled.Java.ArtifactID, defaultArtifactIDPrefix+"secretmanager"; got != want {
		t.Errorf("filled.Java.ArtifactID = %q, want %q", got, want)
	}
	if filled.APIs[0].Java == nil || filled.APIs[0].Java.GenerateGAPIC == nil || !*filled.APIs[0].Java.GenerateGAPIC {
		t.Errorf("filled.APIs[0].Java.GenerateGAPIC = %v, want true", filled.APIs[0].Java)
	}

	tidied, err := Tidy(filled)
	if err != nil {
		t.Fatalf("Tidy() unexpected error: %v", err)
	}
	if tidied.Java != nil {
		t.Errorf("Tidy() expected library.Java to be cleared when all defaults, got %+v", tidied.Java)
	}
	if tidied.APIs[0].Java != nil {
		t.Errorf("Tidy() expected API.Java to be cleared when all defaults, got %+v", tidied.APIs[0].Java)
	}
}

func TestValidate(t *testing.T) {
	validCfg := &config.Config{
		Libraries: []*config.Library{
			{
				Name:    "kms",
				Version: "0.1.0-SNAPSHOT",
				APIs: []*config.API{
					{Path: "google/cloud/kms/v1"},
				},
			},
		},
	}
	if err := Validate(validCfg); err != nil {
		t.Errorf("Validate(validCfg) unexpected error: %v", err)
	}

	invalidCfg := &config.Config{
		Libraries: []*config.Library{
			{
				Name:    "kms",
				Version: "invalid-version",
			},
		},
	}
	if err := Validate(invalidCfg); err == nil {
		t.Error("Validate(invalidCfg) expected error for invalid version, got nil")
	}
}

func TestClean(t *testing.T) {
	tmpDir := t.TempDir()
	libDir := filepath.Join(tmpDir, "clients", "kms")

	// Generated directories & files that should be removed
	kotlinDir := filepath.Join(libDir, "src", "main", "kotlin", "com", "example")
	if err := os.MkdirAll(kotlinDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kotlinDir, "Client.kt"), []byte("class Client"), 0644); err != nil {
		t.Fatal(err)
	}
	javaDir := filepath.Join(libDir, "src", "main", "java", "com", "example")
	if err := os.MkdirAll(javaDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(javaDir, "ServiceGrpc.java"), []byte("class ServiceGrpc"), 0644); err != nil {
		t.Fatal(err)
	}
	protoDir := filepath.Join(libDir, "src", "main", "proto")
	if err := os.MkdirAll(protoDir, 0755); err != nil {
		t.Fatal(err)
	}
	protoFile := filepath.Join(protoDir, "service.proto")
	if err := os.WriteFile(protoFile, []byte("syntax = \"proto3\";"), 0644); err != nil {
		t.Fatal(err)
	}
	buildFile := filepath.Join(libDir, "build.gradle.kts")
	if err := os.WriteFile(buildFile, []byte("plugins {}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Preserved directories & files (test)
	testDir := filepath.Join(libDir, "src", "test", "kotlin")
	if err := os.MkdirAll(testDir, 0755); err != nil {
		t.Fatal(err)
	}
	testFile := filepath.Join(testDir, "ClientTest.kt")
	if err := os.WriteFile(testFile, []byte("class ClientTest"), 0644); err != nil {
		t.Fatal(err)
	}

	lib := &config.Library{
		Name:   "kms",
		Output: libDir,
	}
	if err := Clean(lib); err != nil {
		t.Fatalf("Clean() unexpected error: %v", err)
	}

	if _, err := os.Stat(kotlinDir); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed", kotlinDir)
	}
	if _, err := os.Stat(javaDir); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed", javaDir)
	}
	if _, err := os.Stat(protoFile); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed", protoFile)
	}
	if _, err := os.Stat(buildFile); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed", buildFile)
	}
	if _, err := os.Stat(testFile); err != nil {
		t.Errorf("expected %s to be preserved, got err: %v", testFile, err)
	}
	_ = cmp.Diff("", "")
}

func TestInstallGRPCPluginNotConfigured(t *testing.T) {
	for _, test := range []struct {
		name  string
		tools *config.Tools
	}{
		{name: "nil tools", tools: nil},
		{name: "no maven tools", tools: &config.Tools{}},
		{
			name: "unrelated maven tool",
			tools: &config.Tools{
				Maven: []*config.MavenTool{{Name: "google-java-format"}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := installGRPCPlugin(t.Context(), test.tools)
			if err != nil {
				t.Fatalf("installGRPCPlugin() unexpected error: %v", err)
			}
			if got != "" {
				t.Errorf("installGRPCPlugin() = %q, want empty", got)
			}
		})
	}
}

func TestGRPCPluginClassifier(t *testing.T) {
	got, err := grpcPluginClassifier()
	if err != nil {
		t.Skipf("unsupported test platform: %v", err)
	}
	if !strings.Contains(got, "-") {
		t.Errorf("grpcPluginClassifier() = %q, want <os>-<arch>", got)
	}
}
