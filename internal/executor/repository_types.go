package executor

type RepositoryRemote struct {
	Name     string `json:"name"`
	URL      string `json:"url,omitempty"`
	PushURL  string `json:"push_url,omitempty"`
	Identity string `json:"identity,omitempty"`
}

type RepositoryWorktree struct {
	Path     string `json:"path"`
	Head     string `json:"head,omitempty"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	Bare     bool   `json:"bare,omitempty"`
	Locked   bool   `json:"locked,omitempty"`
	Prunable bool   `json:"prunable,omitempty"`
}

type RepositoryRemoteVerification struct {
	Attempted   bool   `json:"attempted"`
	Succeeded   bool   `json:"succeeded"`
	Exists      bool   `json:"exists"`
	Remote      string `json:"remote,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Head        string `json:"head,omitempty"`
	MatchesHead bool   `json:"matches_head,omitempty"`
	ErrorCode   string `json:"error_code,omitempty"`
}

type RepositoryState struct {
	Path               string                       `json:"path"`
	Root               string                       `json:"root"`
	GitDir             string                       `json:"git_dir"`
	CommonGitDir       string                       `json:"common_git_dir"`
	MainWorktree       string                       `json:"main_worktree,omitempty"`
	LinkedWorktree     bool                         `json:"linked_worktree"`
	Head               string                       `json:"head"`
	Branch             string                       `json:"branch,omitempty"`
	Detached           bool                         `json:"detached"`
	Upstream           string                       `json:"upstream,omitempty"`
	Ahead              int                          `json:"ahead,omitempty"`
	Behind             int                          `json:"behind,omitempty"`
	Dirty              bool                         `json:"dirty"`
	Staged             int                          `json:"staged"`
	Unstaged           int                          `json:"unstaged"`
	Untracked          int                          `json:"untracked"`
	Conflicts          int                          `json:"conflicts"`
	StatusTruncated    bool                         `json:"status_truncated,omitempty"`
	Operations         []string                     `json:"operations,omitempty"`
	Remotes            []RepositoryRemote           `json:"remotes,omitempty"`
	Worktrees          []RepositoryWorktree         `json:"worktrees,omitempty"`
	RemoteVerification RepositoryRemoteVerification `json:"remote_verification"`
}

type RepositoryDiscoveryError struct {
	Path      string `json:"path"`
	ErrorCode string `json:"error_code"`
}

type workspaceTestHooks struct {
	remoteHead           func(remoteURL, remoteRef string) (head string, exists bool, err error)
	beforeWorktreeRemove func()
}
