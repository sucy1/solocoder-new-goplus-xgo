/*
 * Copyright (c) 2021-2021 The XGo Authors (xgo.dev). All rights reserved.
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

// Package test implements the “gop test” command.
package test

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/goplus/gogen"
	"github.com/goplus/xgo/cl"
	"github.com/goplus/xgo/cmd/internal/base"
	"github.com/goplus/xgo/tool"
	"github.com/goplus/xgo/x/gocmd"
	"github.com/goplus/xgo/x/xgoprojs"
)

// gop test
var Cmd = &base.Command{
	UsageLine: "gop test [-debug] [packages]",
	Short:     "Test XGo packages",
}

var (
	flag      = &Cmd.Flag
	flagDebug = flag.Bool("debug", false, "print debug information")
)

func init() {
	Cmd.Run = runCmd
}

func runCmd(cmd *base.Command, args []string) {
	pass := PassTestFlags(cmd)
	err := flag.Parse(args)
	if err != nil {
		log.Fatalln("parse input arguments failed:", err)
	}

	pattern := flag.Args()
	if len(pattern) == 0 {
		pattern = []string{"."}
	}

	projs, err := xgoprojs.ParseAll(pattern...)
	if err != nil {
		log.Panicln("xgoprojs.ParseAll:", err)
	}

	if *flagDebug {
		gogen.SetDebug(gogen.DbgFlagAll &^ gogen.DbgFlagComments)
		cl.SetDebug(cl.DbgFlagAll)
		cl.SetDisableRecover(true)
	}

	conf, err := tool.NewDefaultConf(".", 0, pass.Tags())
	if err != nil {
		log.Panicln("tool.NewDefaultConf:", err)
	}
	defer conf.UpdateCache()

	confCmd := conf.NewGoCmdConf()
	var coverAutoAdded bool
	confCmd.Flags, coverAutoAdded = processCoverFlags(pass.Args)
	for _, proj := range projs {
		test(proj, conf, confCmd)
	}

	generateCoverHTML(confCmd, coverAutoAdded)
}

func processCoverFlags(args []string) ([]string, bool) {
	hasCover := false
	hasCoverProfile := false
	for _, arg := range args {
		if arg == "-cover=true" || arg == "-cover" {
			hasCover = true
		}
		if strings.HasPrefix(arg, "-coverprofile=") {
			hasCoverProfile = true
		}
	}
	autoAdded := false
	if hasCover && !hasCoverProfile {
		args = append(args, "-coverprofile=coverage.out")
		autoAdded = true
	}
	return args, autoAdded
}

func generateCoverHTML(conf *gocmd.TestConfig, coverAutoAdded bool) {
	if conf == nil {
		return
	}
	hasCover := false
	coverProfile := "coverage.out"
	for _, arg := range conf.Flags {
		if arg == "-cover=true" || arg == "-cover" {
			hasCover = true
		}
		if strings.HasPrefix(arg, "-coverprofile=") {
			coverProfile = arg[len("-coverprofile="):]
		}
	}
	if !hasCover {
		return
	}

	profilePath := coverProfile
	if conf.Dir != "" && !filepath.IsAbs(coverProfile) {
		profilePath = filepath.Join(conf.Dir, coverProfile)
	}

	if _, err := os.Stat(profilePath); os.IsNotExist(err) {
		return
	}

	if isCoverageEmpty(profilePath) {
		if coverAutoAdded {
			os.Remove(profilePath)
		}
		return
	}

	goCmd := conf.GoCmd
	if goCmd == "" {
		goCmd = gocmd.Name()
	}
	htmlPath := strings.TrimSuffix(coverProfile, filepath.Ext(coverProfile)) + ".html"
	htmlCmd := exec.Command(goCmd, "tool", "cover", "-html="+coverProfile, "-o", htmlPath)
	htmlCmd.Dir = conf.Dir
	htmlCmd.Stderr = os.Stderr
	htmlCmd.Stdout = os.Stdout
	if err := htmlCmd.Run(); err == nil {
		fmt.Fprintf(os.Stderr, "coverage report: %s\n", htmlPath)
	}
}

func isCoverageEmpty(profilePath string) bool {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return true
	}
	content := string(data)
	content = strings.TrimSpace(content)
	if content == "" {
		return true
	}
	lines := strings.Split(content, "\n")
	hasCoverage := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "mode:") {
			continue
		}
		hasCoverage = true
		break
	}
	return !hasCoverage
}

func test(proj xgoprojs.Proj, conf *tool.Config, test *gocmd.TestConfig) {
	const flags = tool.GenFlagPrompt
	var obj string
	var err error
	switch v := proj.(type) {
	case *xgoprojs.DirProj:
		obj = v.Dir
		err = tool.TestDir(obj, conf, test, flags)
	case *xgoprojs.PkgPathProj:
		obj = v.Path
		err = tool.TestPkgPath("", v.Path, conf, test, flags)
	case *xgoprojs.FilesProj:
		err = tool.TestFiles(v.Files, conf, test)
	default:
		log.Panicln("`gop test` doesn't support", reflect.TypeOf(v))
	}
	if tool.NotFound(err) {
		fmt.Fprintf(os.Stderr, "gop test %v: not found\n", obj)
	} else if err != nil {
		fmt.Fprintln(os.Stderr, err)
	} else {
		return
	}
	os.Exit(1)
}

// -----------------------------------------------------------------------------
