/*
 * Copyright (c) 2023 The XGo Authors (xgo.dev). All rights reserved.
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

package typesutil

import (
	"fmt"
	goast "go/ast"
	"go/types"
	"path/filepath"
	"strings"

	"github.com/goplus/gogen"
	"github.com/goplus/mod/xgomod"
	"github.com/goplus/xgo/ast"
	"github.com/goplus/xgo/cl"
	"github.com/goplus/xgo/token"
	"github.com/goplus/xgo/x/typesutil/internal/typesutil"
	"github.com/qiniu/x/errors"
	"github.com/qiniu/x/log"
)

// -----------------------------------------------------------------------------

type dbgFlags int

const (
	DbgFlagVerbose dbgFlags = 1 << iota
	DbgFlagPrintError
	DbgFlagDisableRecover
	DbgFlagDefault = DbgFlagVerbose | DbgFlagPrintError
	DbgFlagAll     = DbgFlagDefault | DbgFlagDisableRecover
)

var (
	debugVerbose  bool
	debugPrintErr bool
)

func SetDebug(flags dbgFlags) {
	debugVerbose = (flags & DbgFlagVerbose) != 0
	debugPrintErr = (flags & DbgFlagPrintError) != 0
	if (flags & DbgFlagDisableRecover) != 0 {
		cl.SetDisableRecover(true)
	}
}

// -----------------------------------------------------------------------------

type Project = cl.Project

type Config struct {
	// Types provides type information for the package (required).
	Types *types.Package

	// Fset provides source position information for syntax trees and types (required).
	Fset *token.FileSet

	// WorkingDir is the directory in which to run xgo compiler (optional).
	// If WorkingDir is not set, os.Getwd() is used.
	WorkingDir string

	// Mod represents an XGo module (optional).
	Mod *xgomod.Module

	// If IgnoreFuncBodies is set, skip compiling function bodies (optional).
	IgnoreFuncBodies bool

	// If UpdateGoTypesOverload is set, update go types overload data (optional).
	UpdateGoTypesOverload bool
}

// A Checker maintains the state of the type checker.
// It must be created with NewChecker.
type Checker struct {
	conf    *types.Config
	opts    *Config
	goInfo  *types.Info
	xgoInfo *Info
}

// NewChecker returns a new Checker instance for a given package.
// Package files may be added incrementally via checker.Files.
func NewChecker(conf *types.Config, opts *Config, goInfo *types.Info, xgoInfo *Info) *Checker {
	return &Checker{conf, opts, goInfo, xgoInfo}
}

// Files checks the provided files as part of the checker's package.
func (p *Checker) Files(goFiles []*goast.File, xgoFiles []*ast.File) (err error) {
	opts := p.opts
	pkgTypes := opts.Types
	fset := opts.Fset
	conf := p.conf

	// Save original Error handler and restore it on function exit
	origError := conf.Error
	defer func() {
		conf.Error = origError
	}()

	if len(xgoFiles) == 0 {
		onErr := conf.Error
		if onErr != nil {
			conf.Error = func(err error) {
				if e, ok := convGoErr(err); ok {
					onErr(e)
				}
			}
		}
		checker := types.NewChecker(conf, fset, pkgTypes, p.goInfo)
		return checker.Files(goFiles)
	}
	files := make([]*goast.File, 0, len(goFiles))
	gofs := make(map[string]*goast.File)
	xgofs := make(map[string]*ast.File)
	for _, goFile := range goFiles {
		f := fset.File(goFile.Pos())
		if f == nil {
			continue
		}
		file := f.Name()
		fname := filepath.Base(file)
		if strings.HasPrefix(fname, "xgo_autogen") {
			continue
		}
		gofs[file] = goFile
		files = append(files, goFile)
	}
	for _, xgoFile := range xgoFiles {
		f := fset.File(xgoFile.Pos())
		if f == nil {
			continue
		}
		xgofs[f.Name()] = xgoFile
	}
	if debugVerbose {
		log.Println("typesutil.Check:", pkgTypes.Path(), "xgoFiles =", len(xgofs), "goFiles =", len(gofs))
	}
	pkg := &ast.Package{
		Name:    pkgTypes.Name(),
		Files:   xgofs,
		GoFiles: gofs,
	}
	mod := opts.Mod
	if mod == nil {
		mod = xgomod.Default
	}
	_, err = cl.NewPackage(pkgTypes.Path(), pkg, &cl.Config{
		Types:          pkgTypes,
		Fset:           fset,
		LookupClass:    mod.LookupClass,
		Importer:       conf.Importer,
		Recorder:       NewRecorder(p.xgoInfo),
		NoFileLine:     true,
		NoAutoGenMain:  true,
		NoSkipConstant: true,
		Outline:        opts.IgnoreFuncBodies,
	})
	if err != nil {
		if onErr := conf.Error; onErr != nil {
			if list, ok := err.(errors.List); ok {
				for _, e := range list {
					if ce, ok := convErr(fset, e); ok {
						onErr(ce)
					}
				}
			} else if ce, ok := convErr(fset, err); ok {
				onErr(ce)
			} else {
				onErr(err)
			}
		}
		if debugPrintErr {
			log.Println("typesutil.Check err:", err)
			log.SingleStack()
		}
		// don't return even if err != nil
	}
	if len(files) > 0 {
		onErr := conf.Error
		if onErr != nil {
			conf.Error = func(err error) {
				if e, ok := convGoErr(err); ok {
					onErr(e)
				}
			}
		}
		scope := pkgTypes.Scope()
		objMap := DeleteObjects(scope, files)
		checker := types.NewChecker(conf, fset, pkgTypes, p.goInfo)
		err = checker.Files(files)
		// TODO(xsw): how to process error?
		CorrectTypesInfo(scope, objMap, p.xgoInfo.Uses)
		if opts.UpdateGoTypesOverload {
			gogen.InitXGoPackage(pkgTypes)
		}
	}

	if origError != nil {
		checkConcurrencySafety(fset, pkgTypes, goFiles, xgoFiles, origError)
	}
	return
}

type astIdent interface {
	comparable
	ast.Node
}

type objMapT = map[types.Object]types.Object

// CorrectTypesInfo corrects types info to avoid there are two instances for the same Go object.
func CorrectTypesInfo[Ident astIdent](scope *types.Scope, objMap objMapT, uses map[Ident]types.Object) {
	for o := range objMap {
		objMap[o] = scope.Lookup(o.Name())
	}
	for id, old := range uses {
		if new := objMap[old]; new != nil {
			uses[id] = new
		}
	}
}

// DeleteObjects deletes all objects defined in Go files and returns deleted objects.
func DeleteObjects(scope *types.Scope, files []*goast.File) objMapT {
	objMap := make(objMapT)
	for _, f := range files {
		for _, decl := range f.Decls {
			switch v := decl.(type) {
			case *goast.GenDecl:
				for _, spec := range v.Specs {
					switch v := spec.(type) {
					case *goast.ValueSpec:
						for _, name := range v.Names {
							scopeDelete(objMap, scope, name.Name)
						}
					case *goast.TypeSpec:
						scopeDelete(objMap, scope, v.Name.Name)
					}
				}
			case *goast.FuncDecl:
				if v.Recv == nil {
					scopeDelete(objMap, scope, v.Name.Name)
				}
			}
		}
	}
	return objMap
}

func convErr(fset *token.FileSet, e error) (ret Error, ok bool) {
	switch v := e.(type) {
	case *gogen.CodeError:
		ret.Pos, ret.End, ret.Msg = v.Pos, v.End, v.Msg
	case *gogen.MatchError:
		if v.Src != nil {
			ret.Pos, ret.End = v.Src.Pos(), v.Src.End()
		}
		ret.Msg = v.Message("")
	case *gogen.ImportError:
		ret.Pos, ret.End, ret.Msg = v.Pos, v.End, v.Err.Error()
	default:
		return
	}
	ret.Fset, ok = fset, true
	return
}

func convGoErr(e error) (ret Error, ok bool) {
	if v, ok := e.(types.Error); ok {
		ret.Fset, ret.Pos, ret.Msg, ret.Soft = v.Fset, v.Pos, v.Msg, v.Soft
		code, _, end, ok := typesutil.GetErrorGo116(&v)
		if ok {
			ret.Code = Code(code)
			ret.End = end
		}
	}
	return ret, true
}

func scopeDelete(objMap map[types.Object]types.Object, scope *types.Scope, name string) {
	if o := typesutil.ScopeDelete(scope, name); o != nil {
		objMap[o] = nil
	}
}

// -----------------------------------------------------------------------------
// Concurrency safety checks
// -----------------------------------------------------------------------------

type concurrencyChecker struct {
	fset     *token.FileSet
	typesPkg *types.Package
	onErr    func(error)
	info     *types.Info

	sharedVars     map[string]bool
	sharedMaps     map[string]bool
	syncMapVars    map[string]bool
	mutexVars      map[string]bool
	mutexPaths     map[string]bool
	waitGroupVars  map[string]bool
	onceVars       map[string]bool
	condVars       map[string]bool
	chanVars       map[string]bool
	inGoStmt       bool
	lockDepth      int
	pendingUnlocks []deferredUnlock
}

type deferredUnlock struct {
	lockIdent string
	pos       token.Pos
}

func newConcurrencyChecker(fset *token.FileSet, pkg *types.Package, onErr func(error)) *concurrencyChecker {
	c := &concurrencyChecker{
		fset:          fset,
		typesPkg:      pkg,
		onErr:         onErr,
		sharedVars:    make(map[string]bool),
		sharedMaps:    make(map[string]bool),
		syncMapVars:   make(map[string]bool),
		mutexVars:     make(map[string]bool),
		mutexPaths:    make(map[string]bool),
		waitGroupVars: make(map[string]bool),
		onceVars:      make(map[string]bool),
		condVars:      make(map[string]bool),
		chanVars:      make(map[string]bool),
	}
	c.collectSharedState()
	return c
}

func (c *concurrencyChecker) collectSharedState() {
	if c.typesPkg == nil {
		return
	}
	scope := c.typesPkg.Scope()
	for _, name := range scope.Names() {
		obj := scope.Lookup(name)
		if vr, ok := obj.(*types.Var); ok && !vr.IsField() {
			typ := vr.Type()
			c.collectVarTypes(name, typ)
		} else if tn, ok := obj.(*types.TypeName); ok {
			if tn.Pkg() == c.typesPkg {
				if named, ok := tn.Type().(*types.Named); ok {
					if st, ok := named.Underlying().(*types.Struct); ok {
						c.collectStructFieldLocks(name, st)
					}
				}
			}
		}
	}
}

func (c *concurrencyChecker) collectVarTypes(name string, typ types.Type) {
	switch {
	case isMutexType(typ):
		c.mutexVars[name] = true
		c.mutexPaths[name] = true
	case isSyncMapType(typ):
		c.syncMapVars[name] = true
		c.sharedVars[name] = true
	case isWaitGroupType(typ):
		c.waitGroupVars[name] = true
	case isOnceType(typ):
		c.onceVars[name] = true
	case isCondType(typ):
		c.condVars[name] = true
	case isChanType(typ):
		c.chanVars[name] = true
	case isMapType(typ):
		c.sharedMaps[name] = true
		c.sharedVars[name] = true
	default:
		c.sharedVars[name] = true
	}
}

func (c *concurrencyChecker) collectStructFieldLocks(typeName string, st *types.Struct) {
	for i := 0; i < st.NumFields(); i++ {
		field := st.Field(i)
		fieldName := field.Name()
		typ := field.Type()

		fullPath := typeName + "." + fieldName
		switch {
		case isMutexType(typ):
			c.mutexPaths[fullPath] = true
		case isSyncMapType(typ):
			c.syncMapVars[fullPath] = true
		}

		if c.typesPkg != nil {
			if ptr, ok := typ.(*types.Pointer); ok {
				if named, ok := ptr.Elem().(*types.Named); ok {
					if named.Obj().Pkg() == c.typesPkg {
						if st2, ok := named.Underlying().(*types.Struct); ok {
							c.collectStructFieldLocks(fullPath, st2)
						}
					}
				}
			} else if named, ok := typ.(*types.Named); ok {
				if named.Obj().Pkg() == c.typesPkg {
					if st2, ok := named.Underlying().(*types.Struct); ok {
						c.collectStructFieldLocks(fullPath, st2)
					}
				}
			}
		}
	}
}

func isMutexType(typ types.Type) bool {
	named, ok := getUnderlyingNamed(typ)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	pkgPath := obj.Pkg().Path()
	typeName := obj.Name()
	if pkgPath == "sync" {
		if typeName == "Mutex" || typeName == "RWMutex" {
			return true
		}
	}
	return false
}

func isSyncMapType(typ types.Type) bool {
	named, ok := getUnderlyingNamed(typ)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == "sync" && obj.Name() == "Map"
}

func isWaitGroupType(typ types.Type) bool {
	named, ok := getUnderlyingNamed(typ)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == "sync" && obj.Name() == "WaitGroup"
}

func isOnceType(typ types.Type) bool {
	named, ok := getUnderlyingNamed(typ)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == "sync" && obj.Name() == "Once"
}

func isCondType(typ types.Type) bool {
	named, ok := getUnderlyingNamed(typ)
	if !ok {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	return obj.Pkg().Path() == "sync" && obj.Name() == "Cond"
}

func getUnderlyingNamed(typ types.Type) (*types.Named, bool) {
	if ptr, ok := typ.(*types.Pointer); ok {
		return getUnderlyingNamed(ptr.Elem())
	}
	named, ok := typ.(*types.Named)
	return named, ok
}

func isMapType(typ types.Type) bool {
	if ptr, ok := typ.(*types.Pointer); ok {
		return isMapType(ptr.Elem())
	}
	_, ok := typ.Underlying().(*types.Map)
	return ok
}

func isChanType(typ types.Type) bool {
	if ptr, ok := typ.(*types.Pointer); ok {
		return isChanType(ptr.Elem())
	}
	_, ok := typ.Underlying().(*types.Chan)
	return ok
}

func (c *concurrencyChecker) CheckFiles(goFiles []*goast.File, xgoFiles []*ast.File) {
	for _, f := range goFiles {
		goast.Inspect(f, c.visitGoNode)
	}
	for _, f := range xgoFiles {
		ast.Inspect(f, c.visitXGoNode)
	}
}

func (c *concurrencyChecker) visitGoNode(n goast.Node) bool {
	switch v := n.(type) {
	case *goast.GoStmt:
		c.inGoStmt = true
		oldLockDepth := c.lockDepth
		oldUnlocks := c.pendingUnlocks
		c.lockDepth = 0
		c.pendingUnlocks = nil
		goast.Inspect(v.Call, c.visitGoNodeInGoroutine)
		c.applyDeferredUnlocks()
		c.inGoStmt = false
		c.lockDepth = oldLockDepth
		c.pendingUnlocks = oldUnlocks
		return false

	case *goast.DeferStmt:
		c.handleDeferUnlock(v)
		return true

	case *goast.AssignStmt:
		for _, lhs := range v.Lhs {
			c.checkSharedVarWrite(lhs)
		}

	case *goast.IncDecStmt:
		c.checkSharedVarWrite(v.X)

	case *goast.IndexExpr:
		c.checkSharedMapAccess(v)

	case *goast.CallExpr:
		c.handleLockCall(v)
		c.checkSyncMapAccess(v)
		c.checkSharedVarMethodCall(v)
	}
	return true
}

func (c *concurrencyChecker) visitGoNodeInGoroutine(n goast.Node) bool {
	switch v := n.(type) {
	case *goast.AssignStmt:
		for _, lhs := range v.Lhs {
			c.checkSharedVarWrite(lhs)
		}

	case *goast.IncDecStmt:
		c.checkSharedVarWrite(v.X)

	case *goast.IndexExpr:
		c.checkSharedMapAccess(v)

	case *goast.CallExpr:
		c.handleLockCall(v)
		c.checkSyncMapAccess(v)
		c.checkSharedVarMethodCall(v)

	case *goast.DeferStmt:
		c.handleDeferUnlock(v)

	case *goast.SelectorExpr:
		c.checkSharedVarSelectorAccess(v)

	case *goast.Ident:
		if c.sharedVars[v.Name] && c.lockDepth == 0 {
			c.reportError(v.Pos(), UnsynchronizedSharedVar,
				fmt.Sprintf("shared variable %s accessed in goroutine without synchronization", v.Name))
		}
	}
	return true
}

func (c *concurrencyChecker) visitXGoNode(n ast.Node) bool {
	switch v := n.(type) {
	case *ast.GoStmt:
		c.inGoStmt = true
		oldLockDepth := c.lockDepth
		oldUnlocks := c.pendingUnlocks
		c.lockDepth = 0
		c.pendingUnlocks = nil
		ast.Inspect(v.Call, c.visitXGoNodeInGoroutine)
		c.applyDeferredUnlocks()
		c.inGoStmt = false
		c.lockDepth = oldLockDepth
		c.pendingUnlocks = oldUnlocks
		return false
	}
	return true
}

func (c *concurrencyChecker) visitXGoNodeInGoroutine(n ast.Node) bool {
	return true
}

func (c *concurrencyChecker) checkSharedVarWrite(expr goast.Expr) {
	if !c.inGoStmt || c.lockDepth > 0 {
		return
	}

	varName := c.getVarName(expr)
	if varName != "" && c.sharedVars[varName] {
		c.reportError(expr.Pos(), UnsynchronizedSharedVar,
			fmt.Sprintf("shared variable %s modified in goroutine without synchronization", varName))
	}

	c.checkFieldWrite(expr)
}

func (c *concurrencyChecker) checkSharedVarSelectorAccess(sel *goast.SelectorExpr) {
	if !c.inGoStmt || c.lockDepth > 0 {
		return
	}
	varName := c.getVarName(sel.X)
	if varName != "" && c.sharedVars[varName] {
		c.reportError(sel.Pos(), UnsynchronizedSharedVar,
			fmt.Sprintf("shared variable %s accessed in goroutine without synchronization", varName))
	}
}

func (c *concurrencyChecker) getVarName(expr goast.Expr) string {
	switch v := expr.(type) {
	case *goast.Ident:
		return v.Name
	case *goast.StarExpr:
		return c.getVarName(v.X)
	case *goast.ParenExpr:
		return c.getVarName(v.X)
	}
	return ""
}

func (c *concurrencyChecker) checkFieldWrite(expr goast.Expr) {
	if sel, ok := expr.(*goast.SelectorExpr); ok {
		varName := c.getVarName(sel.X)
		if varName != "" && c.sharedVars[varName] {
			c.reportError(sel.Pos(), UnsynchronizedSharedVar,
				fmt.Sprintf("shared variable %s.%s modified in goroutine without synchronization", varName, sel.Sel.Name))
		}
	}
}

func (c *concurrencyChecker) checkSharedMapAccess(expr *goast.IndexExpr) {
	if !c.inGoStmt || c.lockDepth > 0 {
		return
	}
	varName := c.getVarName(expr.X)
	if varName != "" && c.sharedMaps[varName] {
		c.reportError(expr.Pos(), UnsynchronizedMapAccess,
			fmt.Sprintf("shared map %s accessed in goroutine without synchronization", varName))
	}

	if sel, ok := expr.X.(*goast.SelectorExpr); ok {
		fullPath := c.getFullSelectorPath(sel)
		if fullPath != "" && c.isProtectedByLock(fullPath) {
			return
		}
		mapName := c.getVarName(sel.X)
		if mapName != "" && c.sharedMaps[mapName] {
			c.reportError(expr.Pos(), UnsynchronizedMapAccess,
				fmt.Sprintf("shared map %s.%s accessed in goroutine without synchronization", mapName, sel.Sel.Name))
		}
	}
}

func (c *concurrencyChecker) getFullSelectorPath(sel *goast.SelectorExpr) string {
	var parts []string
	current := goast.Expr(sel)
	for {
		if s, ok := current.(*goast.SelectorExpr); ok {
			parts = append([]string{s.Sel.Name}, parts...)
			current = s.X
		} else if id, ok := current.(*goast.Ident); ok {
			parts = append([]string{id.Name}, parts...)
			break
		} else {
			return ""
		}
	}
	return strings.Join(parts, ".")
}

func (c *concurrencyChecker) isProtectedByLock(varPath string) bool {
	if c.lockDepth > 0 {
		return true
	}
	return false
}

func (c *concurrencyChecker) handleLockCall(call *goast.CallExpr) {
	sel, ok := call.Fun.(*goast.SelectorExpr)
	if !ok {
		return
	}

	lockIdent := c.getLockIdentifier(sel.X)
	if lockIdent == "" {
		return
	}

	switch sel.Sel.Name {
	case "Lock", "RLock":
		c.lockDepth++
	case "Unlock", "RUnlock":
		if c.lockDepth > 0 {
			c.lockDepth--
		}
	}
}

func (c *concurrencyChecker) handleDeferUnlock(deferStmt *goast.DeferStmt) {
	call := deferStmt.Call
	sel, ok := call.Fun.(*goast.SelectorExpr)
	if !ok {
		return
	}
	if sel.Sel.Name == "Unlock" || sel.Sel.Name == "RUnlock" {
		lockIdent := c.getLockIdentifier(sel.X)
		if lockIdent != "" {
			c.pendingUnlocks = append(c.pendingUnlocks, deferredUnlock{
				lockIdent: lockIdent,
				pos:       deferStmt.Pos(),
			})
		}
	}
}

func (c *concurrencyChecker) applyDeferredUnlocks() {
	for range c.pendingUnlocks {
		if c.lockDepth > 0 {
			c.lockDepth--
		}
	}
	c.pendingUnlocks = nil
}

func (c *concurrencyChecker) getLockIdentifier(expr goast.Expr) string {
	switch v := expr.(type) {
	case *goast.Ident:
		if c.mutexVars[v.Name] {
			return v.Name
		}
		fullPath := v.Name
		if c.mutexPaths[fullPath] {
			return fullPath
		}
	case *goast.SelectorExpr:
		fullPath := c.getFullSelectorPath(v)
		if fullPath != "" && (c.mutexVars[fullPath] || c.mutexPaths[fullPath]) {
			return fullPath
		}
		if id, ok := v.X.(*goast.Ident); ok {
			if c.mutexVars[id.Name+"."+v.Sel.Name] || c.mutexPaths[id.Name+"."+v.Sel.Name] {
				return id.Name + "." + v.Sel.Name
			}
			if c.mutexVars[id.Name] || c.mutexPaths[id.Name] {
				return id.Name
			}
		}
	case *goast.StarExpr:
		return c.getLockIdentifier(v.X)
	}
	return ""
}

func (c *concurrencyChecker) checkSyncMapAccess(call *goast.CallExpr) {
	if !c.inGoStmt || c.lockDepth > 0 {
		return
	}
	sel, ok := call.Fun.(*goast.SelectorExpr)
	if !ok {
		return
	}

	recvPath := c.getFullSelectorPath(sel)
	recvName := c.getVarName(sel.X)
	varName := ""
	if recvName != "" && c.syncMapVars[recvName] {
		varName = recvName
	} else if recvPath != "" && c.syncMapVars[recvPath] {
		varName = recvPath
	}

	if varName != "" {
		switch sel.Sel.Name {
		case "Load", "Store", "Delete", "LoadOrStore", "LoadAndDelete", "Range", "CompareAndSwap", "CompareAndDelete", "Swap":
			c.reportError(call.Pos(), UnsynchronizedMapAccess,
				fmt.Sprintf("sync.Map %s accessed in goroutine via %s (sync.Map is concurrency-safe, but check usage)", varName, sel.Sel.Name))
		}
	}
}

func (c *concurrencyChecker) checkSharedVarMethodCall(call *goast.CallExpr) {
	if !c.inGoStmt || c.lockDepth > 0 {
		return
	}
	sel, ok := call.Fun.(*goast.SelectorExpr)
	if !ok {
		return
	}
	recvName := c.getVarName(sel.X)
	if recvName != "" && c.sharedVars[recvName] {
		c.reportError(call.Pos(), UnsynchronizedSharedVar,
			fmt.Sprintf("shared variable %s method %s called in goroutine without synchronization", recvName, sel.Sel.Name))
	}
}

func (c *concurrencyChecker) reportError(pos token.Pos, code Code, msg string) {
	if c.onErr != nil {
		c.onErr(Error{
			Fset: c.fset,
			Pos:  pos,
			Msg:  msg,
			Code: code,
			Soft: true,
		})
	}
}

// checkConcurrencySafety runs concurrency safety checks on the provided files.
func checkConcurrencySafety(fset *token.FileSet, pkg *types.Package, goFiles []*goast.File, xgoFiles []*ast.File, onErr func(error)) {
	c := newConcurrencyChecker(fset, pkg, onErr)
	c.CheckFiles(goFiles, xgoFiles)
}
