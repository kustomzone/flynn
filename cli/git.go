package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/flynn/go-docopt"
	"github.com/flynn/flynn/Godeps/_workspace/src/github.com/kardianos/osext"
	cfg "github.com/flynn/flynn/cli/config"
)

var gitRepo *bool

func inGitRepo() bool {
	if gitRepo != nil {
		return *gitRepo
	}
	b := exec.Command("git", "rev-parse", "--git-dir").Run() == nil
	gitRepo = &b
	return b
}

const gitURLSuffix = ".git"

func gitURL(conf *cfg.Cluster, app string) string {
	var prefix string
	if conf.Domain != "" {
		prefix = gitHTTPURLPre(conf.Domain)
	} else if conf.GitHost != "" {
		prefix = gitSSHURLPre(conf.GitHost)
	}
	return prefix + app + gitURLSuffix
}

func gitSSHURLPre(gitHost string) string {
	return "ssh://git@" + gitHost + "/"
}

func gitHTTPURLPre(domain string) string {
	return fmt.Sprintf("https://git.%s/", domain)
}

func mapOutput(out []byte, sep, term string) map[string]string {
	m := make(map[string]string)
	lines := strings.Split(string(out), term)
	for _, line := range lines[:len(lines)-1] { // omit trailing ""
		parts := strings.SplitN(line, sep, 2)
		m[parts[0]] = parts[1]
	}
	return m
}

type remoteApp struct {
	Cluster *cfg.Cluster
	Name    string
}

func gitRemoteNames() (results []string, err error) {
	b, err := exec.Command("git", "remote").Output()
	if err != nil {
		return nil, err
	}

	s := bufio.NewScanner(bytes.NewBuffer(b))
	s.Split(bufio.ScanWords)

	for s.Scan() {
		by := s.Bytes()
		f := bytes.Fields(by)

		results = append(results, string(f[0]))
	}

	if err = s.Err(); err != nil {
		return nil, err
	}

	return
}

func gitRemotes() (map[string]remoteApp, error) {
	b, err := exec.Command("git", "remote", "-v").Output()
	if err != nil {
		return nil, err
	}
	return parseGitRemoteOutput(b)
}

func appFromGitURL(remote string) *remoteApp {
	for _, s := range config.Clusters {
		if flagCluster != "" && s.Name != flagCluster {
			continue
		}

		var prefix string
		if s.Domain != "" {
			prefix = gitHTTPURLPre(s.Domain)
		} else if s.GitHost != "" {
			prefix = gitSSHURLPre(s.GitHost)
		} else {
			continue
		}

		if strings.HasPrefix(remote, prefix) && strings.HasSuffix(remote, gitURLSuffix) {
			return &remoteApp{s, remote[len(prefix) : len(remote)-len(gitURLSuffix)]}
		}
	}
	return nil
}

func parseGitRemoteOutput(b []byte) (results map[string]remoteApp, err error) {
	s := bufio.NewScanner(bytes.NewBuffer(b))
	s.Split(bufio.ScanLines)

	results = make(map[string]remoteApp)

	for s.Scan() {
		by := s.Bytes()
		f := bytes.Fields(by)
		if len(f) != 3 || string(f[2]) != "(push)" {
			// this should have 3 tuples + be a push remote, skip it if not
			continue
		}

		if app := appFromGitURL(string(f[1])); app != nil {
			results[string(f[0])] = *app
		}
	}
	if err = s.Err(); err != nil {
		return nil, err
	}
	return
}

func remoteFromGitConfig() string {
	b, err := exec.Command("git", "config", "flynn.remote").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type multipleRemotesError []string

func (remotes multipleRemotesError) Error() string {
	return "error: Multiple apps listed in git remotes, please specify one with the global -a option to disambiguate.\n\nAvailable Flynn remotes:\n" + strings.Join(remotes, "\n")
}

func appFromGitRemote(remote string) (*remoteApp, error) {
	if remote != "" {
		b, err := exec.Command("git", "config", "remote."+remote+".url").Output()
		if err != nil {
			if isNotFound(err) {
				wdir, _ := os.Getwd()
				return nil, fmt.Errorf("could not find git remote "+remote+" in %s", wdir)
			}
			return nil, err
		}

		out := strings.TrimSpace(string(b))

		app := appFromGitURL(out)
		if app == nil {
			return nil, fmt.Errorf("could not find app name in " + remote + " git remote")
		}
		return app, nil
	}

	// no remote specified, see if there is a single Flynn app remote
	remotes, err := gitRemotes()
	if err != nil {
		return nil, nil // hide this error
	}
	if len(remotes) > 1 {
		err := make(multipleRemotesError, 0, len(remotes))
		for r := range remotes {
			err = append(err, r)
		}
		return nil, err
	}
	for _, v := range remotes {
		return &v, nil
	}
	return nil, fmt.Errorf("no apps in git remotes")
}

func isNotFound(err error) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		if ws, ok := ee.ProcessState.Sys().(syscall.WaitStatus); ok {
			return ws.ExitStatus() == 1
		}
	}
	return false
}

func caCertDir() string {
	return filepath.Join(cfg.Dir(), "ca-certs")
}

func gitConfig(args ...string) error {
	args = append([]string{"config", "--global"}, args...)
	cmd := exec.Command("git", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("error %q running %q: %q", err, strings.Join(cmd.Args, " "), out)
	}
	return nil
}

func writeGlobalGitConfig(domain, caFile string) error {
	if err := gitConfig(fmt.Sprintf("http.https://git.%s.sslCAInfo", domain), caFile); err != nil {
		return err
	}
	self, err := osext.Executable()
	if err != nil {
		return err
	}
	if err := gitConfig(fmt.Sprintf("credential.https://git.%s.helper", domain), self+" git-credentials"); err != nil {
		return err
	}
	return nil
}

func removeGlobalGitConfig(domain string) {
	for _, k := range []string{
		fmt.Sprintf("http.https://git.%s", domain),
		fmt.Sprintf("credential.https://git.%s", domain),
	} {
		gitConfig("--remove-section", k)
	}
}

func init() {
	register("git-credentials", runGitCredentials, "usage: flynn git-credentials <operation>")
}

func runGitCredentials(args *docopt.Args) error {
	if args.String["<operation>"] != "get" {
		return nil
	}

	detailBytes, _ := ioutil.ReadAll(os.Stdin)
	details := make(map[string]string)
	for _, l := range bytes.Split(detailBytes, []byte("\n")) {
		kv := bytes.SplitN(l, []byte("="), 2)
		if len(kv) == 2 {
			details[string(kv[0])] = string(kv[1])
		}
	}

	if details["protocol"] != "https" {
		return nil
	}
	if err := readConfig(); err != nil {
		return nil
	}

	var cluster *cfg.Cluster
	domain := strings.TrimPrefix(details["host"], "git.")
	for _, c := range config.Clusters {
		if c.Domain == domain {
			cluster = c
			break
		}
	}
	if cluster == nil {
		return nil
	}

	fmt.Printf("protocol=https\nusername=user\nhost=%s\npassword=%s\n", details["host"], cluster.Key)
	return nil
}
