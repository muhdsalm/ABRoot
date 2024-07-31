package core

/*	License: GPLv3
	Authors:
		Mirko Brombin <mirko@fabricators.ltd>
		Vanilla OS Contributors <https://github.com/vanilla-os/>
	Copyright: 2024
	Description:
		ABRoot is utility which provides full immutability and
		atomicity to a Linux system, by transacting between
		two root filesystems. Updates are performed using OCI
		images, to ensure that the system is always in a
		consistent state.
*/

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vanilla-os/abroot/settings"
)

// PackageManager struct
type PackageManager struct {
	dryRun  bool
	baseDir string
	Status  ABRootPkgManagerStatus
}

// Common Package manager paths
const (
	PackagesBaseDir             = "/etc/abroot"
	PkgManagerUserAgreementFile = "/etc/abroot/ABPkgManager.userAgreement"
	DryRunPackagesBaseDir       = "/tmp/abroot"
	PackagesAddFile             = "packages.add"
	PackagesRemoveFile          = "packages.remove"
	PackagesUnstagedFile        = "packages.unstaged"
)

// Package manager operations
const (
	ADD    = "+"
	REMOVE = "-"
)

// Package manager statuses
const (
	PKG_MNG_DISABLED      = 0
	PKG_MNG_ENABLED       = 1
	PKG_MNG_REQ_AGREEMENT = 2
)

// ABRootPkgManagerStatus represents the status of the package manager
// in the ABRoot configuration file
type ABRootPkgManagerStatus int

// An unstaged package is a package that is waiting to be applied
// to the next root.
//
// Every time a `pkg apply` or `upgrade` operation
// is executed, all unstaged packages are consumed and added/removed
// in the next root.
type UnstagedPackage struct {
	Name, Status string
}

// NewPackageManager returns a new PackageManager struct
func NewPackageManager(dryRun bool) (*PackageManager, error) {
	PrintVerboseInfo("PackageManager.NewPackageManager", "running...")

	baseDir := PackagesBaseDir
	if dryRun {
		baseDir = DryRunPackagesBaseDir
	}

	err := os.MkdirAll(baseDir, 0o755)
	if err != nil {
		PrintVerboseErr("PackageManager.NewPackageManager", 0, err)
		return nil, err
	}

	_, err = os.Stat(filepath.Join(baseDir, PackagesAddFile))
	if err != nil {
		err = os.WriteFile(
			filepath.Join(baseDir, PackagesAddFile),
			[]byte(""),
			0o644,
		)
		if err != nil {
			PrintVerboseErr("PackageManager.NewPackageManager", 1, err)
			return nil, err
		}
	}

	_, err = os.Stat(filepath.Join(baseDir, PackagesRemoveFile))
	if err != nil {
		err = os.WriteFile(
			filepath.Join(baseDir, PackagesRemoveFile),
			[]byte(""),
			0o644,
		)
		if err != nil {
			PrintVerboseErr("PackageManager.NewPackageManager", 2, err)
			return nil, err
		}
	}

	_, err = os.Stat(filepath.Join(baseDir, PackagesUnstagedFile))
	if err != nil {
		err = os.WriteFile(
			filepath.Join(baseDir, PackagesUnstagedFile),
			[]byte(""),
			0o644,
		)
		if err != nil {
			PrintVerboseErr("PackageManager.NewPackageManager", 3, err)
			return nil, err
		}
	}

	// here we convert settings.Cnf.IPkgMngStatus to an ABRootPkgManagerStatus
	// for easier understanding in the code
	var status ABRootPkgManagerStatus
	switch settings.Cnf.IPkgMngStatus {
	case PKG_MNG_REQ_AGREEMENT:
		status = PKG_MNG_REQ_AGREEMENT
	case PKG_MNG_ENABLED:
		status = PKG_MNG_ENABLED
	default:
		status = PKG_MNG_DISABLED
	}

	return &PackageManager{dryRun, baseDir, status}, nil
}

// Add adds a package to the packages.add file
func (p *PackageManager) Add(pkg string) error {
	PrintVerboseInfo("PackageManager.Add", "running...")

	// Check for package manager status and user agreement
	err := p.CheckStatus()
	if err != nil {
		PrintVerboseErr("PackageManager.Add", 0, err)
		return err
	}

	// Check if package was removed before
	packageWasRemoved := false
	removedIndex := -1
	pkgsRemove, err := p.GetRemovePackages()
	if err != nil {
		PrintVerboseErr("PackageManager.Add", 2.1, err)
		return err
	}
	for i, rp := range pkgsRemove {
		if rp == pkg {
			packageWasRemoved = true
			removedIndex = i
			break
		}
	}

	// packages that have been removed by the user aren't always in the repo
	if !packageWasRemoved {
		// Check if package exists in repo
		for _, _pkg := range strings.Split(pkg, " ") {
			err := p.ExistsInRepo(_pkg)
			if err != nil {
				PrintVerboseErr("PackageManager.Add", 0, err)
				return err
			}
		}
	}

	// Add to unstaged packages first
	upkgs, err := p.GetUnstagedPackages()
	if err != nil {
		PrintVerboseErr("PackageManager.Add", 1, err)
		return err
	}
	upkgs = append(upkgs, UnstagedPackage{pkg, ADD})
	err = p.writeUnstagedPackages(upkgs)
	if err != nil {
		PrintVerboseErr("PackageManager.Add", 2, err)
		return err
	}

	// If package was removed by the user, simply remove it from packages.remove
	// Unstaged will take care of the rest
	if packageWasRemoved {
		pkgsRemove = append(pkgsRemove[:removedIndex], pkgsRemove[removedIndex+1:]...)
		PrintVerboseInfo("PackageManager.Add", "unsetting manually removed package")
		return p.writeRemovePackages(pkgsRemove)
	}

	// Abort if package is already added
	pkgsAdd, err := p.GetAddPackages()
	if err != nil {
		PrintVerboseErr("PackageManager.Add", 3, err)
		return err
	}
	for _, p := range pkgsAdd {
		if p == pkg {
			PrintVerboseInfo("PackageManager.Add", "package already added")
			return nil
		}
	}

	pkgsAdd = append(pkgsAdd, pkg)

	PrintVerboseInfo("PackageManager.Add", "writing packages.add")
	return p.writeAddPackages(pkgsAdd)
}

// Remove either removes a manually added package from packages.add or adds
// a package to be deleted into packages.remove
func (p *PackageManager) Remove(pkg string) error {
	PrintVerboseInfo("PackageManager.Remove", "running...")

	// Check for package manager status and user agreement
	err := p.CheckStatus()
	if err != nil {
		PrintVerboseErr("PackageManager.Remove", 0, err)
		return err
	}

	// Check if package exists in repo
	// FIXME: this should also check if the package is actually installed
	// in the system, not just if it exists in the repo. Since this is a distro
	// specific feature, I'm leaving it as is for now.
	err = p.ExistsInRepo(pkg)
	if err != nil {
		PrintVerboseErr("PackageManager.Remove", 1, err)
		return err
	}

	// Add to unstaged packages first
	upkgs, err := p.GetUnstagedPackages()
	if err != nil {
		PrintVerboseErr("PackageManager.Remove", 2, err)
		return err
	}
	upkgs = append(upkgs, UnstagedPackage{pkg, REMOVE})
	err = p.writeUnstagedPackages(upkgs)
	if err != nil {
		PrintVerboseErr("PackageManager.Remove", 3, err)
		return err
	}

	// If package was added by the user, simply remove it from packages.add
	// Unstaged will take care of the rest
	pkgsAdd, err := p.GetAddPackages()
	if err != nil {
		PrintVerboseErr("PackageManager.Remove", 4, err)
		return err
	}
	for i, ap := range pkgsAdd {
		if ap == pkg {
			pkgsAdd = append(pkgsAdd[:i], pkgsAdd[i+1:]...)
			PrintVerboseInfo("PackageManager.Remove", "removing manually added package")
			return p.writeAddPackages(pkgsAdd)
		}
	}

	// Abort if package is already removed
	pkgsRemove, err := p.GetRemovePackages()
	if err != nil {
		PrintVerboseErr("PackageManager.Remove", 5, err)
		return err
	}
	for _, p := range pkgsRemove {
		if p == pkg {
			PrintVerboseInfo("PackageManager.Remove", "package already removed")
			return nil
		}
	}

	pkgsRemove = append(pkgsRemove, pkg)

	// Otherwise, add package to packages.remove
	PrintVerboseInfo("PackageManager.Remove", "writing packages.remove")
	return p.writeRemovePackages(pkgsRemove)
}

// GetAddPackages returns the packages in the packages.add file
func (p *PackageManager) GetAddPackages() ([]string, error) {
	PrintVerboseInfo("PackageManager.GetAddPackages", "running...")
	return p.getPackages(PackagesAddFile)
}

// GetRemovePackages returns the packages in the packages.remove file
func (p *PackageManager) GetRemovePackages() ([]string, error) {
	PrintVerboseInfo("PackageManager.GetRemovePackages", "running...")
	return p.getPackages(PackagesRemoveFile)
}

// GetUnstagedPackages returns the package changes that are yet to be applied
func (p *PackageManager) GetUnstagedPackages() ([]UnstagedPackage, error) {
	PrintVerboseInfo("PackageManager.GetUnstagedPackages", "running...")
	pkgs, err := p.getPackages(PackagesUnstagedFile)
	if err != nil {
		PrintVerboseErr("PackageManager.GetUnstagedPackages", 0, err)
		return nil, err
	}

	unstagedList := []UnstagedPackage{}
	for _, line := range pkgs {
		if line == "" || line == "\n" {
			continue
		}

		splits := strings.SplitN(line, " ", 2)
		unstagedList = append(unstagedList, UnstagedPackage{splits[1], splits[0]})
	}

	return unstagedList, nil
}

// GetUnstagedPackagesPlain returns the package changes that are yet to be applied
// as strings
func (p *PackageManager) GetUnstagedPackagesPlain() ([]string, error) {
	PrintVerboseInfo("PackageManager.GetUnstagedPackagesPlain", "running...")
	pkgs, err := p.GetUnstagedPackages()
	if err != nil {
		PrintVerboseErr("PackageManager.GetUnstagedPackagesPlain", 0, err)
		return nil, err
	}

	unstagedList := []string{}
	for _, pkg := range pkgs {
		unstagedList = append(unstagedList, pkg.Name)
	}

	return unstagedList, nil
}

// ClearUnstagedPackages removes all packages from the unstaged list
func (p *PackageManager) ClearUnstagedPackages() error {
	PrintVerboseInfo("PackageManager.ClearUnstagedPackages", "running...")
	return p.writeUnstagedPackages([]UnstagedPackage{})
}

// GetAddPackagesString returns the packages in the packages.add file as a string
func (p *PackageManager) GetAddPackagesString(sep string) (string, error) {
	PrintVerboseInfo("PackageManager.GetAddPackagesString", "running...")
	pkgs, err := p.GetAddPackages()
	if err != nil {
		PrintVerboseErr("PackageManager.GetAddPackagesString", 0, err)
		return "", err
	}

	PrintVerboseInfo("PackageManager.GetAddPackagesString", "done")
	return strings.Join(pkgs, sep), nil
}

// GetRemovePackagesString returns the packages in the packages.remove file as a string
func (p *PackageManager) GetRemovePackagesString(sep string) (string, error) {
	PrintVerboseInfo("PackageManager.GetRemovePackagesString", "running...")
	pkgs, err := p.GetRemovePackages()
	if err != nil {
		PrintVerboseErr("PackageManager.GetRemovePackagesString", 0, err)
		return "", err
	}

	PrintVerboseInfo("PackageManager.GetRemovePackagesString", "done")
	return strings.Join(pkgs, sep), nil
}

func (p *PackageManager) getPackages(file string) ([]string, error) {
	PrintVerboseInfo("PackageManager.getPackages", "running...")

	pkgs := []string{}
	f, err := os.Open(filepath.Join(p.baseDir, file))
	if err != nil {
		PrintVerboseErr("PackageManager.getPackages", 0, err)
		return pkgs, err
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		PrintVerboseErr("PackageManager.getPackages", 1, err)
		return pkgs, err
	}

	pkgs = strings.Split(strings.TrimSpace(string(b)), "\n")

	PrintVerboseInfo("PackageManager.getPackages", "returning packages")
	return pkgs, nil
}

func (p *PackageManager) writeAddPackages(pkgs []string) error {
	PrintVerboseInfo("PackageManager.writeAddPackages", "running...")
	return p.writePackages(PackagesAddFile, pkgs)
}

func (p *PackageManager) writeRemovePackages(pkgs []string) error {
	PrintVerboseInfo("PackageManager.writeRemovePackages", "running...")
	return p.writePackages(PackagesRemoveFile, pkgs)
}

func (p *PackageManager) writeUnstagedPackages(pkgs []UnstagedPackage) error {
	PrintVerboseInfo("PackageManager.writeUnstagedPackages", "running...")

	// create slice without redundant entries
	pkgsCleaned := []UnstagedPackage{}
	for _, pkg := range pkgs {
		isDuplicate := false
		for iCmp, pkgCmp := range pkgsCleaned {
			if pkg.Name == pkgCmp.Name {
				isDuplicate = true

				// remove complement (+ then - or - then +)
				if pkg.Status != pkgCmp.Status {
					pkgsCleaned = append(pkgsCleaned[:iCmp], pkgsCleaned[iCmp+1:]...)
				}

				break
			}
		}

		// don't add duplicate
		if !isDuplicate {
			pkgsCleaned = append(pkgsCleaned, pkg)
		}
	}

	pkgFmt := []string{}
	for _, pkg := range pkgsCleaned {
		pkgFmt = append(pkgFmt, fmt.Sprintf("%s %s", pkg.Status, pkg.Name))
	}

	return p.writePackages(PackagesUnstagedFile, pkgFmt)
}

func (p *PackageManager) writePackages(file string, pkgs []string) error {
	PrintVerboseInfo("PackageManager.writePackages", "running...")

	f, err := os.Create(filepath.Join(p.baseDir, file))
	if err != nil {
		PrintVerboseErr("PackageManager.writePackages", 0, err)
		return err
	}
	defer f.Close()

	for _, pkg := range pkgs {
		if pkg == "" {
			continue
		}

		_, err = fmt.Fprintf(f, "%s\n", pkg)
		if err != nil {
			PrintVerboseErr("PackageManager.writePackages", 1, err)
			return err
		}
	}

	PrintVerboseInfo("PackageManager.writePackages", "packages written")
	return nil
}

func (p *PackageManager) processApplyPackages() (string, string) {
	PrintVerboseInfo("PackageManager.processApplyPackages", "running...")

	unstaged, err := p.GetUnstagedPackages()
	if err != nil {
		PrintVerboseErr("PackageManager.processApplyPackages", 0, err)
	}

	var addPkgs, removePkgs []string
	for _, pkg := range unstaged {
		switch pkg.Status {
		case ADD:
			addPkgs = append(addPkgs, pkg.Name)
		case REMOVE:
			removePkgs = append(removePkgs, pkg.Name)
		}
	}

	finalAddPkgs := ""
	if len(addPkgs) > 0 {
		finalAddPkgs = fmt.Sprintf("%s %s", settings.Cnf.IPkgMngAdd, strings.Join(addPkgs, " "))
	}

	finalRemovePkgs := ""
	if len(removePkgs) > 0 {
		finalRemovePkgs = fmt.Sprintf("%s %s", settings.Cnf.IPkgMngRm, strings.Join(removePkgs, " "))
	}

	return finalAddPkgs, finalRemovePkgs
}

func (p *PackageManager) processUpgradePackages() (string, string) {
	addPkgs, err := p.GetAddPackagesString(" ")
	if err != nil {
		PrintVerboseErr("PackageManager.processUpgradePackages", 0, err)
		return "", ""
	}

	removePkgs, err := p.GetRemovePackagesString(" ")
	if err != nil {
		PrintVerboseErr("PackageManager.processUpgradePackages", 1, err)
		return "", ""
	}

	if len(addPkgs) == 0 && len(removePkgs) == 0 {
		PrintVerboseInfo("PackageManager.processUpgradePackages", "no packages to install or remove")
		return "", ""
	}

	finalAddPkgs := ""
	if addPkgs != "" {
		finalAddPkgs = fmt.Sprintf("%s %s", settings.Cnf.IPkgMngAdd, addPkgs)
	}

	finalRemovePkgs := ""
	if removePkgs != "" {
		finalRemovePkgs = fmt.Sprintf("%s %s", settings.Cnf.IPkgMngRm, removePkgs)
	}

	return finalAddPkgs, finalRemovePkgs
}

func (p *PackageManager) GetFinalCmd(operation ABSystemOperation) string {
	PrintVerboseInfo("PackageManager.GetFinalCmd", "running...")

	var finalAddPkgs, finalRemovePkgs string
	if operation == APPLY {
		finalAddPkgs, finalRemovePkgs = p.processApplyPackages()
	} else {
		finalAddPkgs, finalRemovePkgs = p.processUpgradePackages()
	}

	cmd := ""
	if finalAddPkgs != "" && finalRemovePkgs != "" {
		cmd = fmt.Sprintf("%s && %s", finalAddPkgs, finalRemovePkgs)
	} else if finalAddPkgs != "" {
		cmd = finalAddPkgs
	} else if finalRemovePkgs != "" {
		cmd = finalRemovePkgs
	}

	// No need to add pre/post hooks to an empty operation
	if cmd == "" {
		return cmd
	}

	preExec := settings.Cnf.IPkgMngPre
	postExec := settings.Cnf.IPkgMngPost
	if preExec != "" {
		cmd = fmt.Sprintf("%s && %s", preExec, cmd)
	}
	if postExec != "" {
		cmd = fmt.Sprintf("%s && %s", cmd, postExec)
	}

	PrintVerboseInfo("PackageManager.GetFinalCmd", "returning cmd: "+cmd)
	return cmd
}

func (p *PackageManager) getSummary() (string, error) {
	if p.CheckStatus() != nil {
		return "", nil
	}

	addPkgs, err := p.GetAddPackages()
	if err != nil {
		if errors.Is(err, &os.PathError{}) {
			addPkgs = []string{}
		} else {
			return "", err
		}
	}
	removePkgs, err := p.GetRemovePackages()
	if err != nil {
		if errors.Is(err, &os.PathError{}) {
			removePkgs = []string{}
		} else {
			return "", err
		}
	}

	// GetPackages returns slices with one empty element if there are no packages
	if len(addPkgs) == 1 && addPkgs[0] == "" {
		addPkgs = []string{}
	}
	if len(removePkgs) == 1 && removePkgs[0] == "" {
		removePkgs = []string{}
	}

	summary := ""

	for _, pkg := range addPkgs {
		summary += "+ " + pkg + "\n"
	}
	for _, pkg := range removePkgs {
		summary += "- " + pkg + "\n"
	}

	return summary, nil
}

// WriteSummaryToFile writes added and removed packages to summaryFilePath
//
// added packages get the + prefix, while removed packages get the - prefix
func (p *PackageManager) WriteSummaryToFile(summaryFilePath string) error {
	summary, err := p.getSummary()
	if err != nil {
		return err
	}
	if summary == "" {
		return nil
	}
	summaryFile, err := os.Create(summaryFilePath)
	if err != nil {
		return err
	}
	defer summaryFile.Close()
	err = summaryFile.Chmod(0o644)
	if err != nil {
		return err
	}
	_, err = summaryFile.WriteString(summary)
	if err != nil {
		return err
	}

	return nil
}

// assertPkgMngApiSetUp checks whether the repo API is properly configured.
// If a configuration exists but is malformed, returns an error.
func assertPkgMngApiSetUp() (bool, error) {
	if settings.Cnf.IPkgMngApi == "" {
		PrintVerboseInfo("PackageManager.assertPkgMngApiSetUp", "no API url set, will not check if package exists. This could lead to errors")
		return false, nil
	}

	_, err := url.ParseRequestURI(settings.Cnf.IPkgMngApi)
	if err != nil {
		return false, fmt.Errorf("PackageManager.assertPkgMngApiSetUp: Value set as API url (%s) is not a valid URL", settings.Cnf.IPkgMngApi)
	}

	if !strings.Contains(settings.Cnf.IPkgMngApi, "{packageName}") {
		return false, fmt.Errorf("PackageManager.assertPkgMngApiSetUp: API url does not contain {packageName} placeholder. ABRoot is probably misconfigured, please report the issue to the maintainers of the distribution")
	}

	PrintVerboseInfo("PackageManager.assertPkgMngApiSetUp", "Repo is set up properly")
	return true, nil
}

func (p *PackageManager) ExistsInRepo(pkg string) error {
	PrintVerboseInfo("PackageManager.ExistsInRepo", "running...")

	ok, err := assertPkgMngApiSetUp()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	url := strings.Replace(settings.Cnf.IPkgMngApi, "{packageName}", pkg, 1)
	PrintVerboseInfo("PackageManager.ExistsInRepo", "checking if package exists in repo: "+url)

	resp, err := http.Get(url)
	if err != nil {
		PrintVerboseErr("PackageManager.ExistsInRepo", 0, err)
		return err
	}

	if resp.StatusCode != 200 {
		PrintVerboseInfo("PackageManager.ExistsInRepo", "package does not exist in repo")
		return fmt.Errorf("package does not exist in repo: %s", pkg)
	}

	PrintVerboseInfo("PackageManager.ExistsInRepo", "package exists in repo")
	return nil
}

// GetRepoContentsForPkg retrieves package information from the repository API
func GetRepoContentsForPkg(pkg string) (map[string]interface{}, error) {
	PrintVerboseInfo("PackageManager.GetRepoContentsForPkg", "running...")

	ok, err := assertPkgMngApiSetUp()
	if err != nil {
		return map[string]interface{}{}, err
	}
	if !ok {
		return map[string]interface{}{}, errors.New("PackageManager.GetRepoContentsForPkg: no API url set, cannot query package information")
	}

	url := strings.Replace(settings.Cnf.IPkgMngApi, "{packageName}", pkg, 1)
	PrintVerboseInfo("PackageManager.GetRepoContentsForPkg", "fetching package information in: "+url)

	resp, err := http.Get(url)
	if err != nil {
		PrintVerboseErr("PackageManager.GetRepoContentsForPkg", 0, err)
		return map[string]interface{}{}, err
	}

	contents, err := io.ReadAll(resp.Body)
	if err != nil {
		PrintVerboseErr("PackageManager.GetRepoContentsForPkg", 1, err)
		return map[string]interface{}{}, err
	}

	pkgInfo := map[string]interface{}{}
	err = json.Unmarshal(contents, &pkgInfo)
	if err != nil {
		PrintVerboseErr("PackageManager.GetRepoContentsForPkg", 2, err)
		return map[string]interface{}{}, err
	}

	return pkgInfo, nil
}

// AcceptUserAgreement sets the package manager status to enabled
func (p *PackageManager) AcceptUserAgreement() error {
	PrintVerboseInfo("PackageManager.AcceptUserAgreement", "running...")

	if p.Status != PKG_MNG_REQ_AGREEMENT {
		PrintVerboseInfo("PackageManager.AcceptUserAgreement", "package manager is not in agreement mode")
		return nil
	}

	err := os.WriteFile(
		PkgManagerUserAgreementFile,
		[]byte(time.Now().String()),
		0o644,
	)
	if err != nil {
		PrintVerboseErr("PackageManager.AcceptUserAgreement", 0, err)
		return err
	}

	return nil
}

// GetUserAgreementStatus returns if the user has accepted the package manager
// agreement or not
func (p *PackageManager) GetUserAgreementStatus() bool {
	PrintVerboseInfo("PackageManager.GetUserAgreementStatus", "running...")

	if p.Status != PKG_MNG_REQ_AGREEMENT {
		PrintVerboseInfo("PackageManager.GetUserAgreementStatus", "package manager is not in agreement mode")
		return true
	}

	_, err := os.Stat(PkgManagerUserAgreementFile)
	if err != nil {
		PrintVerboseInfo("PackageManager.GetUserAgreementStatus", "user has not accepted the agreement")
		return false
	}

	PrintVerboseInfo("PackageManager.GetUserAgreementStatus", "user has accepted the agreement")
	return true
}

// CheckStatus checks if the package manager is enabled or not
func (p *PackageManager) CheckStatus() error {
	PrintVerboseInfo("PackageManager.CheckStatus", "running...")

	// Check if package manager is enabled
	if p.Status == PKG_MNG_DISABLED {
		PrintVerboseInfo("PackageManager.CheckStatus", "package manager is disabled")
		return nil
	}

	// Check if user has accepted the package manager agreement
	if p.Status == PKG_MNG_REQ_AGREEMENT {
		if !p.GetUserAgreementStatus() {
			PrintVerboseInfo("PackageManager.CheckStatus", "package manager agreement not accepted")
			err := errors.New("package manager agreement not accepted")
			return err
		}
	}

	PrintVerboseInfo("PackageManager.CheckStatus", "package manager is enabled")
	return nil
}
