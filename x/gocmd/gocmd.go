/*
 * Copyright (c) 2021 The XGo Authors (xgo.dev). All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package gocmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/goplus/mod/env"
	"github.com/goplus/xgo/x/xgoenv"
)

type XGoEnv = env.XGo

type Config struct {
	XGo   *XGoEnv
	GoCmd string
	Flags []string
	Run   func(cmd *exec.Cmd) error
	Dir   string
}

// -----------------------------------------------------------------------------

func doWithArgs(dir, op string, conf *Config, args ...string) (err error) {
	if conf == nil {
		conf = new(Config)
	}
	goCmd := conf.GoCmd
	if goCmd == "" {
		goCmd = Name()
	}
	exargs := make([]string, 1, 16)
	exargs[0] = op
	exargs = appendLdflags(exargs, conf.XGo)
	exargs = append(exargs, conf.Flags...)
	exargs = append(exargs, args...)

	workDir := dir
	if workDir == "" {
		workDir = conf.Dir
	}

	cmd := exec.Command(goCmd, exargs...)
	cmd.Dir = workDir
	cmd.Env = setupEnv(workDir, exargs)

	if op == "build" || op == "install" || op == "test" || op == "run" {
		if err = autoModDownload(workDir, conf, exargs); err != nil {
			return
		}
	}

	run := conf.Run
	if run == nil {
		run = runCmd
	}
	return run(cmd)
}

func setupEnv(dir string, args []string) []string {
	env := os.Environ()

	if goos := getFlagValue(args, "GOOS"); goos == "" {
		goos = os.Getenv("GOOS")
	} else {
		env = setEnv(env, "GOOS", goos)
	}
	if goarch := getFlagValue(args, "GOARCH"); goarch == "" {
		goarch = os.Getenv("GOARCH")
	} else {
		env = setEnv(env, "GOARCH", goarch)
	}

	if goos == "android" {
		env = setupAndroidNDK(env, goarch)
	}

	if gocache := os.Getenv("GOCACHE"); gocache != "" {
		env = setEnv(env, "GOCACHE", gocache)
	}

	return env
}

func getFlagValue(args []string, name string) string {
	prefix := "-" + name + "="
	for _, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			return arg[len(prefix):]
		}
	}
	return ""
}

func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	for i, e := range env {
		if strings.HasPrefix(e, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func setupAndroidNDK(env []string, goarch string) []string {
	ndkRoot := os.Getenv("ANDROID_NDK_HOME")
	if ndkRoot == "" {
		ndkRoot = os.Getenv("NDK_ROOT")
	}
	if ndkRoot == "" {
		if home := os.Getenv("HOME"); home != "" {
			candidates := []string{
				filepath.Join(home, "Android", "Sdk", "ndk"),
				filepath.Join(home, "AppData", "Local", "Android", "Sdk", "ndk"),
				"/opt/android-ndk",
				"/usr/local/lib/android/sdk/ndk",
			}
			for _, cand := range candidates {
				if info, err := os.Stat(cand); err == nil && info.IsDir() {
					entries, err := os.ReadDir(cand)
					if err == nil && len(entries) > 0 {
						ndkRoot = filepath.Join(cand, entries[0].Name())
						break
					}
				}
			}
		}
	}
	if ndkRoot == "" {
		return env
	}

	var clangPrefix string
	switch goarch {
	case "arm64":
		clangPrefix = "aarch64-linux-android"
	case "arm":
		clangPrefix = "armv7a-linux-androideabi"
	case "386":
		clangPrefix = "i686-linux-android"
	case "amd64":
		clangPrefix = "x86_64-linux-android"
	default:
		clangPrefix = "aarch64-linux-android"
	}

	var toolchain string
	if runtimeGOOS := os.Getenv("GOHOSTOS"); runtimeGOOS == "windows" || runtimeGOOS == "" {
		if _, err := os.Stat(filepath.Join(ndkRoot, "toolchains", "llvm", "prebuilt", "windows-x86_64")); err == nil {
			toolchain = filepath.Join(ndkRoot, "toolchains", "llvm", "prebuilt", "windows-x86_64")
		}
	}
	if toolchain == "" {
		if _, err := os.Stat(filepath.Join(ndkRoot, "toolchains", "llvm", "prebuilt", "darwin-x86_64")); err == nil {
			toolchain = filepath.Join(ndkRoot, "toolchains", "llvm", "prebuilt", "darwin-x86_64")
		}
	}
	if toolchain == "" {
		toolchain = filepath.Join(ndkRoot, "toolchains", "llvm", "prebuilt", "linux-x86_64")
	}

	binDir := filepath.Join(toolchain, "bin")
	cc := filepath.Join(binDir, clangPrefix+"21-clang")
	cxx := filepath.Join(binDir, clangPrefix+"21-clang++")

	env = setEnv(env, "CC", cc)
	env = setEnv(env, "CXX", cxx)
	env = setEnv(env, "CGO_ENABLED", "1")
	if cgoLdflags := os.Getenv("CGO_LDFLAGS"); cgoLdflags == "" {
		env = setEnv(env, "CGO_LDFLAGS", fmt.Sprintf("-L%s/sysroot/usr/lib/%s", toolchain, clangPrefix))
	}

	return env
}

func autoModDownload(dir string, conf *Config, args []string) error {
	modFlag := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "-mod=") {
			modFlag = arg[5:]
			break
		}
	}
	if modFlag != "mod" {
		return nil
	}

	goModPath := filepath.Join(dir, "go.mod")
	if _, err := os.Stat(goModPath); err != nil {
		return nil
	}

	goSumPath := filepath.Join(dir, "go.sum")
	if _, err := os.Stat(goSumPath); err != nil {
		goCmd := conf.GoCmd
		if goCmd == "" {
			goCmd = Name()
		}
		downloadCmd := exec.Command(goCmd, "mod", "download")
		downloadCmd.Dir = dir
		downloadCmd.Stderr = os.Stderr
		downloadCmd.Stdout = os.Stdout
		return downloadCmd.Run()
	}

	return nil
}

func runCmd(cmd *exec.Cmd) (err error) {
	cmd.Stdin = os.Stdin
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	return cmd.Run()
}

// -----------------------------------------------------------------------------

const (
	ldFlagVersion   = "-X \"github.com/goplus/xgo/env.buildVersion=%s\""
	ldFlagBuildDate = "-X \"github.com/goplus/xgo/env.buildDate=%s\""
	ldFlagGopRoot   = "-X \"github.com/goplus/xgo/env.defaultXGoRoot=%s\""
)

const (
	ldFlagAll = ldFlagVersion + " " + ldFlagBuildDate + " " + ldFlagGopRoot
)

func loadFlags(env *XGoEnv) string {
	return fmt.Sprintf(ldFlagAll, env.Version, env.BuildDate, env.Root)
}

func appendLdflags(exargs []string, env *XGoEnv) []string {
	if env == nil {
		env = xgoenv.Get()
	}
	return append(exargs, "-ldflags", loadFlags(env))
}

// -----------------------------------------------------------------------------

// Name returns name of the go command.
// It returns value of environment variable `XGO_GOCMD` if not empty.
// If not found, it returns `go`.
func Name() string {
	goCmd := os.Getenv("XGO_GOCMD")
	if goCmd == "" {
		goCmd = "go"
	}
	return goCmd
}

// -----------------------------------------------------------------------------
