package main

import (
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestRepositoryLayoutKeepsBuildInputsAndOrganizesSupportFiles(t *testing.T) {
	isolatedGatewayConfig(t)
	root := repoRoot(t)
	trace := newTraceID()
	log.Printf("[Repository regression] traceId=%s stage=start note=Only checks repository layout and embedded resources; does not modify real config or call upstream", trace)
	for _, name := range []string{
		"main.go", "go.mod", "go.sum", "build.ps1", "config.example.json", "README.md",
		"docs/user-guide.md", "docs/contributors.md", "docs/project-layout.md",
		"tests/README.md", "tests/build/build_script.test.ps1", "tests/web/app.test.cjs",
		"scripts/test.ps1", "archive/README.md",
	} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("required project file missing: %s", name)
		}
	}
	for _, name := range []string{"build_test.ps1", "CONTRIBUTORS.md", "web/app_test.cjs"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Errorf("support file was not consolidated: %s", name)
		}
	}
	for _, name := range []string{"web/index.html", "web/app.js", "web/style.css"} {
		if data, err := webFS.ReadFile(name); err != nil || len(data) == 0 {
			t.Errorf("production embedded asset missing: %s", name)
		}
	}
	if _, err := webFS.ReadFile("tests/web/app.test.cjs"); err == nil {
		t.Error("test source must not be embedded into the production executable")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	testName := regexp.MustCompile(`^[a-z0-9]+(?:_[a-z0-9]+)*_test\.go$`)
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "_test.go") {
			count++
			if !testName.MatchString(entry.Name()) {
				t.Errorf("test file does not follow snake_case_test.go: %s", entry.Name())
			}
		}
	}
	if count < 32 {
		t.Errorf("Go package lost test files after organization: count=%d", count)
	}
	log.Printf("[Repository regression] traceId=%s stage=completed goTestFilesInPackage=%d result=checked docsAndStandaloneTestsOrganized=true runtimeResourcesKeptInEmbeddedDir=true", trace, count)
}

func TestRepositoryDocumentationLinksRemainLocalAndResolvable(t *testing.T) {
	isolatedGatewayConfig(t)
	root := repoRoot(t)
	trace := newTraceID()
	log.Printf("[Documentation link regression] traceId=%s stage=start note=Only checks project-internal Markdown links; no internet access", trace)
	link := regexp.MustCompile(`\[[^\]]+\]\(([^)]+)\)`)
	for _, name := range []string{"README.md", "docs/user-guide.md", "docs/project-layout.md", "tests/README.md"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range link.FindAllStringSubmatch(string(data), -1) {
			target := strings.SplitN(match[1], "#", 2)[0]
			if target == "" || strings.Contains(target, "://") {
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), filepath.FromSlash(target))); err != nil {
				t.Errorf("%s links to missing local file: %s", name, target)
			}
		}
	}
	log.Printf("[Documentation link regression] traceId=%s stage=completed result=checked internalLinksNeedNoInternet=true", trace)
}
