package sources

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mit-dci/opencbdc-tctl/common"
	"github.com/mit-dci/opencbdc-tctl/logging"
)

var ErrGitLogOutOfBounds = errors.New("Requested out-of-bounds git log")

type GitLogRecord struct {
	CommitHash       string       `json:"commit"`
	ParentCommitHash string       `json:"parent"`
	Subject          string       `json:"subject"`
	Author           GitLogPerson `json:"author"`
	AuthoredString   string       `json:"authored_date,omitempty"`
	Authored         time.Time    `json:"authored"`
	Committer        GitLogPerson `json:"committer"`
	CommittedString  string       `json:"committed_date,omitempty"`
	Committed        time.Time    `json:"committed"`
}

type GitLogPerson struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type SourcesManager struct {
	gitLog      []GitLogRecord
	sourcesLock sync.Mutex
}

func NewSourcesManager() *SourcesManager {
	s := &SourcesManager{gitLog: []GitLogRecord{}, sourcesLock: sync.Mutex{}}
	return s
}

func sourcesParentDir() string {
	return common.DataDir()
}

func archivePath(commitHash string) (string, error) {
	archiveDir := filepath.Join(common.DataDir(), "archives")
	if _, err := os.Stat(archiveDir); os.IsNotExist(err) {
		err = os.Mkdir(archiveDir, 0755)
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(archiveDir, fmt.Sprintf("%s.tar.gz", commitHash)), nil
}

func BinariesArchivePath(
	commitHash string,
	profilingOrDebugging bool,
) (string, error) {
	if _, err := os.Stat(binariesDir()); os.IsNotExist(err) {
		err = os.Mkdir(binariesDir(), 0755)
		if err != nil {
			return "", err
		}
	}
	if profilingOrDebugging {
		commitHash = fmt.Sprintf("%s-profiling", commitHash)
	}
	return filepath.Join(
		binariesDir(),
		fmt.Sprintf("%s.tar.gz", commitHash),
	), nil
}

func sourcesDirName() string {
	return "sources"
}

func sourcesDir() string {
	dir := filepath.Join(sourcesParentDir(), sourcesDirName())
	return dir
}

func binariesDir() string {
	dir := filepath.Join(common.DataDir(), "binaries")
	return dir
}

func (s *SourcesManager) EnsureSourcesUpdated() error {
	var err error
	if _, err = os.Stat(sourcesDir()); os.IsNotExist(err) {
		err = s.cloneSources()
		if err != nil {
			err = fmt.Errorf("Error cloning sources: %v", err)
		}
	} else {
		err = s.updateSources()
		if err != nil {
			err = fmt.Errorf("Error updating sources: %v", err)
		}
	}
	if err != nil {
		return err
	}
	return s.updateCommitHistory()
}

func (s *SourcesManager) Compile(
	hash string,
	profilingOrDebugging bool,
	progress chan float64,
) error {
	defer func() {
		if progress != nil {
			progress <- 100
			close(progress)
		}
	}()

	binariesPath := filepath.Join(sourcesDir(), "build")
	path, err := BinariesArchivePath(hash, profilingOrDebugging)
	if err != nil {
		return err
	}

	if progress != nil {
		progress <- 1
	}

	s.sourcesLock.Lock()
	defer s.sourcesLock.Unlock()

	if progress != nil {
		progress <- 2
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		// Already exists
		return nil
	}

	cmd := exec.Command("git", "checkout", hash)
	cmd.Dir = sourcesDir()
	err = cmd.Run()
	if err != nil {
		return err
	}
	logging.Infof(
		"[Compile %s-%t]: Checkout complete",
		hash,
		profilingOrDebugging,
	)

	if progress != nil {
		progress <- 5
	}

	cmd = exec.Command("git", "submodule", "sync")
	cmd.Dir = sourcesDir()
	err = cmd.Run()
	if err != nil {
		return err
	}

	cmd = exec.Command("git", "submodule", "update", "--recursive")
	cmd.Dir = sourcesDir()
	err = cmd.Run()
	if err != nil {
		return err
	}
	logging.Infof(
		"[Compile %s-%t]: Update submodules complete",
		hash,
		profilingOrDebugging,
	)

	if progress != nil {
		progress <- 10
	}

	os.RemoveAll(filepath.Join(sourcesDir(), "build"))
	logging.Infof(
		"[Compile %s-%t]: Cleaned build directory",
		hash,
		profilingOrDebugging,
	)

	avoid_legacy_setup := true
	var out []byte
	var env []string
	scriptsDir := filepath.Join(sourcesDir(), "scripts")
	{
		fp := filepath.Join(scriptsDir, "install-build-tools.sh")
		_, err := os.Stat(fp)
		if err == nil {
			cmd = exec.Command("bash", fp)

			cmd.Dir = sourcesDir()
			env := os.Environ()
			if !profilingOrDebugging {
				env = append(env, "BUILD_RELEASE=1")
			}
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				avoid_legacy_setup = false
				return fmt.Errorf("Build-environment setup failed: %v\n\n%v", err, string(out))
			} else {
				logging.Infof(
					"[Compile %s-%t]: Build-environment setup complete",
					hash,
					profilingOrDebugging,
				)
			}
		} else {
			avoid_legacy_setup = false
		}
	}

	{
		fp := filepath.Join(scriptsDir, "setup-dependencies.sh")
		_, err := os.Stat(fp)
		if err == nil {
			cmd = exec.Command("bash", fp)

			cmd.Dir = sourcesDir()
			env := os.Environ()
			if !profilingOrDebugging {
				env = append(env, "BUILD_RELEASE=1")
			}
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if err != nil {
				avoid_legacy_setup = false
				return fmt.Errorf("Dependency installation failed: %v\n\n%v", err, string(out))
			} else {
				logging.Infof(
					"[Compile %s-%t]: Dependency installation complete",
					hash,
					profilingOrDebugging,
				)
			}
		} else {
			avoid_legacy_setup = false
		}
	}

	if !avoid_legacy_setup {
		logging.Infof(
			"[Compile %s-%t]: Attempting to use legacy configuration",
			hash,
			profilingOrDebugging,
		)
		fp := filepath.Join(scriptsDir, "configure.sh")
		_, err := os.Stat(fp)
		if err != nil {
			return fmt.Errorf("Legacy configuration failed: %v\n\n%v", err, string(out))
		}

		cmd = exec.Command("bash", fp)

		cmd.Dir = sourcesDir()
		env := os.Environ()
		if !profilingOrDebugging {
			env = append(env, "BUILD_RELEASE=1")
		}
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("Legacy configuration failed: %v\n\n%v", err, string(out))
		} else {
			logging.Infof(
				"[Compile %s-%t]: Legacy configuration complete",
				hash,
				profilingOrDebugging,
			)
		}
	}

	if progress != nil {
		progress <- 50
	}

	cmd = exec.Command(
		"bash",
		filepath.Join(sourcesDir(), "scripts", "build.sh"),
	)
	cmd.Dir = sourcesDir()
	env = os.Environ()
	if profilingOrDebugging {
		env = append(env, "BUILD_PROFILING=1")
	} else {
		env = append(env, "BUILD_RELEASE=1")
	}
	cmd.Env = env
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Build failed: %v\n\n%v", err, string(out))
	}

	logging.Infof(
		"[Compile %s-%t]: Build script complete",
		hash,
		profilingOrDebugging,
	)
	if progress != nil {
		progress <- 90
	}

	proxy_path := filepath.Join(
		sourcesDir(),
		"src",
		"parsec",
		"agent",
		"runners",
		"evm",
		"rpc_proxy",
	)
	if _, err := os.Stat(proxy_path); !os.IsNotExist(err) {
		logging.Infof(
			"[Compile %s-%t]: Copying parsec/EVM RPC proxy",
			hash,
			profilingOrDebugging,
		)
		dest_proxy_path := filepath.Join(
			binariesPath,
			"src",
			"parsec",
			"agent",
			"runners",
			"evm",
		)
		common.CopyDir(proxy_path, dest_proxy_path)
	}

	return common.CreateArchive(binariesPath, path)
}

type PRData struct {
	Subject        string `json:"subject"`
	AuthoredString string `json:"authored_date"`
}

func (s *SourcesManager) updateCommitHistory() error {
	s.sourcesLock.Lock()
	defer s.sourcesLock.Unlock()
	cmd := exec.Command(
		"git",
		"log",
		`--pretty=format:{%n  $$$commit$$$: $$$%H$$$,%n  $$$parent$$$: $$$%P$$$,%n  $$$subject$$$: $$$%s$$$, %n  $$$author$$$: {%n    $$$name$$$: $$$%aN$$$,%n    $$$email$$$: $$$%aE$$$ },%n  $$$authored_date$$$: $$$%aD$$$%n ,%n  $$$committer$$$: {%n    $$$name$$$: $$$%cN$$$,%n    $$$email$$$: $$$%cE$$$},%n    $$$committed_date$$$: $$$%cD$$$%n%n},`,
	)
	cmd.Dir = sourcesDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"error updating commit history: %v\n%s",
			err,
			string(out),
		)
	}
	outString := string(out[:len(out)-1])
	outString = strings.ReplaceAll(outString, "\"", "\\\"")
	outString = strings.ReplaceAll(outString, "$$$", "\"")
	out = []byte(fmt.Sprintf("[%s]", outString))
	newGitLog := []GitLogRecord{}
	err = json.Unmarshal(out, &newGitLog)
	if err != nil {
		return err
	}

	for i := range newGitLog {
		newGitLog[i].Committed, _ = time.Parse(
			"Mon, 2 Jan 2006 15:04:05 -0700",
			newGitLog[i].CommittedString,
		)
		newGitLog[i].Authored, _ = time.Parse(
			"Mon, 2 Jan 2006 15:04:05 -0700",
			newGitLog[i].AuthoredString,
		)
		newGitLog[i].AuthoredString = ""
		newGitLog[i].CommittedString = ""
	}

	cmd = exec.Command(
		"git",
		"fetch",
		"origin",
		"+refs/pull/*/head:refs/remotes/origin/pr-head/*",
		"--no-recurse-submodules",
	)
	cmd.Dir = sourcesDir()
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("Failed to fetch PRs: %v\n\n%s", err, string(out))
	}

	cmd = exec.Command(
		"git",
		"ls-remote",
		"origin",
	)
	cmd.Dir = sourcesDir()
	out, err = cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"Failed to fetch remote PRs: %v\n\n%s",
			err,
			string(out),
		)
	}
	logging.Infof("ls-remote:\n\n%s", string(out))
	prs := map[int]bool{}
	prHeadCommits := map[int]string{}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		parts := strings.Split(line, "\t")
		if len(parts) == 2 {
			if strings.HasPrefix(parts[1], "refs/pull/") {
				prString := strings.Replace(parts[1], "refs/pull/", "", 1)
				prString = strings.Split(prString, "/")[0]
				pr, err := strconv.Atoi(prString)
				if err == nil {
					if strings.HasSuffix(parts[1], "/merge") {
						prs[pr] = true
						logging.Infof(
							"Detected (at one point) mergeable PR #%d",
							pr,
						)
					} else if strings.HasSuffix(parts[1], "/head") {
						prHeadCommits[pr] = parts[0]
					}
				}
			}
		}
	}

	prGitLogs := make([]GitLogRecord, 0)
	for pr := range prHeadCommits {
		mergeable := prs[pr]
		cmd = exec.Command(
			"git",
			"log",
			"-n",
			"1",
			`--pretty=format:{%n  $$$subject$$$: $$$%s$$$, $$$authored_date$$$: $$$%aD$$$%n }`,
			prHeadCommits[pr],
		)
		cmd.Dir = sourcesDir()
		out, err = cmd.CombinedOutput()
		if err != nil {
			logging.Warnf("git log for PR %d failed: %v", pr, err)
			continue
		}
		outString := strings.ReplaceAll(string(out), "\"", "\\\"")
		outString = strings.ReplaceAll(outString, "$$$", "\"")
		out = []byte(outString)
		var prData PRData
		err = json.Unmarshal(out, &prData)
		if err != nil {
			logging.Warnf(
				"Unmarshal JSON from log for PR %d failed: %v",
				pr,
				err,
			)
			continue
		}
		authored, err := time.Parse(
			"Mon, 2 Jan 2006 15:04:05 -0700",
			prData.AuthoredString,
		)
		if err == nil {
			// Include non-mergeable (or already merged) PRs that are less than
			// 48 hours old, and mergeable PRs that are less than 90 days old
			if authored.After(time.Now().Add(-2*24*time.Hour)) ||
				(mergeable && authored.After(time.Now().Add(-90*24*time.Hour))) {
				// Yes we want this one!
				prGitLogs = append(prGitLogs, GitLogRecord{
					Authored:   authored,
					Committed:  authored,
					Subject:    fmt.Sprintf("PR #%d - %s", pr, prData.Subject),
					CommitHash: prHeadCommits[pr],
				})
			}
		} else {
			logging.Warnf("Authored date for PR %d could not be parsed: %v", pr, err)
		}
	}

	sort.Slice(prGitLogs, func(i, j int) bool {
		return prGitLogs[j].Authored.Before(prGitLogs[i].Authored)
	})
	s.gitLog = append(
		append(append([]GitLogRecord{}, newGitLog[:3]...), prGitLogs...),
		newGitLog[3:]...)

	return nil
}

// FindMostRecentCommitChangingSeeder finds the most recent commit of or before
// the given commit hash that changes the seeder logic. Used to not have to re-
// seed the shards with every commit if the seeder logic hasn't changed.
func (s *SourcesManager) FindMostRecentCommitChangingSeeder(
	commitHash string,
) (string, error) {
	s.sourcesLock.Lock()
	defer s.sourcesLock.Unlock()
	cmd := exec.Command(
		"git",
		"checkout",
		commitHash,
	)
	cmd.Dir = sourcesDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf(
			"Failed to find seeder change commit - [git checkout %s] failed: %v\n\n%s",
			commitHash,
			err,
			string(out),
		)
	}

	cmd = exec.Command(
		"git",
		"log",
		"-1",
		"--pretty=format:%H",
		"tools/shard-seeder/shard-seeder.cpp",
	)
	cmd.Dir = sourcesDir()
	out, err = cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf(
			"Failed to find seeder change commit - failed to execute git log: %v\n\n%s",
			err,
			string(out),
		)
	}
	commitHash = strings.TrimSpace(string(out))

	cmd = exec.Command(
		"git",
		"checkout",
		os.Getenv("TRANSACTION_PROCESSOR_MAIN_BRANCH"),
	)
	cmd.Dir = sourcesDir()
	out, err = cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf(
			"Failed to find seeder change commit - [git checkout %s] failed: %v\n\n%s",
			os.Getenv("TRANSACTION_PROCESSOR_MAIN_BRANCH"),
			err,
			string(out),
		)
	}
	return commitHash, err
}

func (s *SourcesManager) cloneSources() error {
	s.sourcesLock.Lock()
	defer s.sourcesLock.Unlock()

	gitUrl, err := url.Parse(os.Getenv("TRANSACTION_PROCESSOR_REPO_URL"))
	if err != nil {
		return err
	}
	if os.Getenv("TRANSACTION_PROCESSOR_ACCESS_TOKEN") != "" {
		gitUrl.User = url.UserPassword(
			os.Getenv("TRANSACTION_PROCESSOR_ACCESS_TOKEN"),
			"x-oauth-basic",
		)
	}

	cmd := exec.Command(
		"git",
		"clone",
		gitUrl.String(),
		sourcesDirName(),
	)
	cmd.Dir = sourcesParentDir()
	err = cmd.Run()
	if err != nil {
		return fmt.Errorf(
			"Failed to clone sources. Do you have the right token configured? %v",
			err,
		)
	}

	cmd = exec.Command("git", "submodule", "sync")
	cmd.Dir = sourcesDir()
	err = cmd.Run()
	if err != nil {
		return err
	}

	cmd = exec.Command("git", "submodule", "update", "--init", "--recursive")
	cmd.Dir = sourcesDir()
	err = cmd.Run()
	if err != nil {
		return err
	}
	return nil
}

func (s *SourcesManager) updateSources() error {
	s.sourcesLock.Lock()
	defer s.sourcesLock.Unlock()
	cmd := exec.Command(
		"git",
		"checkout",
		os.Getenv("TRANSACTION_PROCESSOR_MAIN_BRANCH"),
	)
	cmd.Dir = sourcesDir()
	out, err := cmd.CombinedOutput()
	if err != nil {
		logging.Errorf("Error on git checkout: %v", string(out))
		return err
	}
	cmd = exec.Command("git", "pull")
	cmd.Dir = sourcesDir()
	out, err = cmd.CombinedOutput()
	if err != nil {
		logging.Errorf("Error on git pull: %v", string(out))
		return err
	}
	return nil
}

func (s *SourcesManager) GetGitLog(
	offset, limit int,
	alwaysIncludeInitial bool,
) ([]GitLogRecord, error) {
	if len(s.gitLog) == 0 {
		return []GitLogRecord{}, nil
	}
	if offset >= len(s.gitLog) {
		return []GitLogRecord{}, ErrGitLogOutOfBounds
	}
	end := offset + limit
	if end > len(s.gitLog) {
		end = len(s.gitLog)
	}

	ret := s.gitLog[offset:end]
	if alwaysIncludeInitial {
		ret = append(ret, s.gitLog[len(s.gitLog)-1])
	}

	return ret, nil
}

func (s *SourcesManager) CommitExists(hash string) bool {
	for _, c := range s.gitLog {
		if c.CommitHash == hash {
			return true
		}
	}
	return false
}
func (s *SourcesManager) ReadCommitArchive(hash string) ([]byte, error) {
	path, err := archivePath(hash)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(path); os.IsNotExist(err) {
		return nil, fmt.Errorf(
			"source archive does not exist. Call MakeCommitArchive first!",
		)
	}
	return ioutil.ReadFile(path)
}

func (s *SourcesManager) MakeCommitArchive(hash string) error {
	s.sourcesLock.Lock()
	defer s.sourcesLock.Unlock()
	path, err := archivePath(hash)
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		// Already exists
		return nil
	}

	cmd := exec.Command("git", "checkout", hash)
	cmd.Dir = sourcesDir()
	err = cmd.Run()
	if err != nil {
		return err
	}

	cmd = exec.Command("git", "submodule", "sync")
	cmd.Dir = sourcesDir()
	err = cmd.Run()
	if err != nil {
		return err
	}

	cmd = exec.Command("git", "submodule", "update", "--recursive")
	cmd.Dir = sourcesDir()
	err = cmd.Run()
	if err != nil {
		return err
	}

	return common.CreateArchive(sourcesDir(), path)
}
