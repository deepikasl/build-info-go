package build

import (
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/jfrog/build-info-go/entities"
	"github.com/jfrog/build-info-go/utils"
)

type GoModule struct {
	containingBuild *Build
	name            string
	goModName       string
	srcPath         string
	goArgs          []string
}

func newGoModule(srcPath string, containingBuild *Build) (*GoModule, error) {
	var err error
	if srcPath == "" {
		srcPath, err = utils.GetProjectRoot()
		if err != nil {
			return nil, err
		}
	}

	// Read module name
	name, err := utils.GetModuleNameByDir(srcPath, containingBuild.logger)
	if err != nil {
		return nil, err
	}

	return &GoModule{name: name, goModName: name, srcPath: srcPath, containingBuild: containingBuild}, nil
}

// Build builds the project, collects its dependencies and saves them in the build-info module.
func (gm *GoModule) Build() (err error) {
	isGoGetCommand := false
	if len(gm.goArgs) > 0 {
		err = utils.RunGo(gm.goArgs)
		if err != nil {
			if _, ok := err.(*exec.ExitError); ok {
				err = errors.New(err.Error())
			}
			return
		}
		isGoGetCommand = gm.goArgs[0] == "get"
	}
	if !gm.containingBuild.buildNameAndNumberProvided() {
		return
	}

	srcPath := gm.srcPath
	biModuleName := gm.name
	if isGoGetCommand {
		if len(gm.goArgs) < 2 {
			// Package name was not supplied. Invalid go get commend
			return errors.New("package name is missing")
		}
		var tempDirPath string
		tempDirPath, err = utils.CreateTempDir()
		if err != nil {
			return
		}
		// Cleanup the temp working directory at the end.
		defer func() {
			e := utils.RemoveTempDir(tempDirPath)
			if err == nil {
				err = e
			}
		}()
		biModuleName, err = gm.handleGoGetCmd(tempDirPath)
		if err != nil {
			return
		}
		srcPath = tempDirPath
	}
	buildInfoDependencies, err := gm.loadDependencies(srcPath, biModuleName)
	if err != nil {
		return
	}

	buildInfoModule := entities.Module{Id: biModuleName, Type: entities.Go, Dependencies: buildInfoDependencies}
	buildInfo := &entities.BuildInfo{Modules: []entities.Module{buildInfoModule}}

	return gm.containingBuild.SaveBuildInfo(buildInfo)
}

// handleGoGetCmd copies the requested package files from the cache to the given destPath and returns the package's module path (the package's name).
func (gm *GoModule) handleGoGetCmd(destPath string) (string, error) {
	var packageName string
	for argIndex := 1; argIndex < len(gm.goArgs); argIndex++ {
		if !strings.HasPrefix(gm.goArgs[argIndex], "-") {
			packageName = gm.goArgs[argIndex]
			break
		}
	}
	if packageName == "" {
		return "", errors.New("package name is missing")
	}
	modulePath, packageFilesPath, err := utils.GetPackagePathAndDir(gm.srcPath, packageName, gm.containingBuild.logger)
	if err != nil {
		return "", err
	}
	// Copy the entire content of the relevant Go pkg directory to the requested destination path.
	err = utils.CopyDir(packageFilesPath, destPath, true, nil)
	if err != nil {
		return "", fmt.Errorf("Couldn't find suitable package files: %s", packageFilesPath)
	}
	return modulePath, nil
}

func (gm *GoModule) SetName(name string) {
	gm.name = name
}

func (gm *GoModule) SetArgs(goArgs []string) {
	gm.goArgs = goArgs
}

func (gm *GoModule) AddArtifacts(artifacts ...entities.Artifact) error {
	if !gm.containingBuild.buildNameAndNumberProvided() {
		return errors.New("build name and build number must be provided in order to add artifacts")
	}
	partial := &entities.Partial{ModuleId: gm.name, ModuleType: entities.Go, Artifacts: artifacts}
	return gm.containingBuild.SavePartialBuildInfo(partial)
}

func (gm *GoModule) loadDependencies(srcPath, parentId string) ([]entities.Dependency, error) {
	cachePath, err := utils.GetCachePath()
	if err != nil {
		return nil, err
	}
	dependenciesGraph, err := utils.GetDependenciesGraph(srcPath, gm.containingBuild.logger)
	if err != nil {
		return nil, err
	}
	dependenciesMap, err := gm.getGoDependencies(cachePath, srcPath)
	if err != nil {
		return nil, err
	}
	emptyRequestedBy := [][]string{{}}
	populateRequestedByField(parentId, emptyRequestedBy, dependenciesMap, dependenciesGraph)
	return dependenciesMapToList(dependenciesMap), nil
}

func (gm *GoModule) getGoDependencies(cachePath, srcPath string) (map[string]entities.Dependency, error) {
	modulesMap, err := utils.GetDependenciesList(srcPath, gm.containingBuild.logger)
	if err != nil || len(modulesMap) == 0 {
		return nil, err
	}
	// Create a map from dependency to parents
	buildInfoDependencies := make(map[string]entities.Dependency)
	for moduleId := range modulesMap {
		// If the path includes capital letters, the Go convention is to use "!" before the letter. The letter itself is in lowercase.
		encodedDependencyId := goModEncode(moduleId)

		// We first check if this dependency has a zip in the local Go cache.
		// If it does not, nil is returned. This seems to be a bug in Go.
		zipPath, err := gm.getPackageZipLocation(cachePath, encodedDependencyId)
		if err != nil {
			return nil, err
		}
		if zipPath == "" {
			continue
		}
		zipDependency, err := populateZip(encodedDependencyId, zipPath)
		if err != nil {
			return nil, err
		}
		buildInfoDependencies[moduleId] = zipDependency
	}
	return buildInfoDependencies, nil
}

// Returns the actual path to the dependency.
// If the path includes capital letters, the Go convention is to use "!" before the letter.
// The letter itself is in lowercase.
func goModEncode(name string) string {
	path := ""
	for _, letter := range name {
		if unicode.IsUpper(letter) {
			path += "!" + strings.ToLower(string(letter))
		} else {
			path += string(letter)
		}
	}
	return path
}

// Returns the path to the package zip file if exists.
func (gm *GoModule) getPackageZipLocation(cachePath, encodedDependencyId string) (string, error) {
	zipPath, err := gm.getPackagePathIfExists(cachePath, encodedDependencyId)
	if err != nil {
		return "", err
	}

	if zipPath != "" {
		return zipPath, nil
	}

	return gm.getPackagePathIfExists(filepath.Dir(cachePath), encodedDependencyId)
}

// Validates that the package zip file exists and returns its path.
func (gm *GoModule) getPackagePathIfExists(cachePath, encodedDependencyId string) (zipPath string, err error) {
	moduleInfo := strings.Split(encodedDependencyId, ":")
	if len(moduleInfo) != 2 {
		gm.containingBuild.logger.Debug("The encoded dependency Id syntax should be 'name:version' but instead got:", encodedDependencyId)
		return "", nil
	}
	dependencyName := moduleInfo[0]
	version := moduleInfo[1]
	zipPath = filepath.Join(cachePath, dependencyName, "@v", version+".zip")
	fileExists, err := utils.IsFileExists(zipPath, true)
	if err != nil {
		return "", errors.New(fmt.Sprintf("Could not find zip binary for dependency '%s' at %s: %s", dependencyName, zipPath, err))
	}
	// Zip binary does not exist, so we skip it by returning a nil dependency.
	if !fileExists {
		gm.containingBuild.logger.Debug("The following file is missing:", zipPath)
		return "", nil
	}
	return zipPath, nil
}

// populateZip adds the zip file as build-info dependency
func populateZip(packageId, zipPath string) (zipDependency entities.Dependency, err error) {
	// Zip file dependency for the build-info
	zipDependency = entities.Dependency{Id: packageId}
	md5, sha1, sha2, err := utils.GetFileChecksums(zipPath)
	if err != nil {
		return
	}
	zipDependency.Type = "zip"
	zipDependency.Checksum = entities.Checksum{Sha1: sha1, Md5: md5, Sha256: sha2}
	return
}

func populateRequestedByField(parentId string, parentRequestedBy [][]string, dependenciesMap map[string]entities.Dependency, dependenciesGraph map[string][]string) {
	for _, childName := range dependenciesGraph[parentId] {
		if childDep, ok := dependenciesMap[childName]; ok {
			for _, requestedBy := range parentRequestedBy {
				childRequestedBy := append([]string{parentId}, requestedBy...)
				childDep.RequestedBy = append(childDep.RequestedBy, childRequestedBy)
			}
			if childDep.NodeHasLoop() {
				continue
			}
			// Reassign map entry with new entry copy
			dependenciesMap[childName] = childDep
			// Run recursive call on child dependencies
			populateRequestedByField(childName, childDep.RequestedBy, dependenciesMap, dependenciesGraph)
		}
	}
}
