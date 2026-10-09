package executor

type EnvironmentDeclaration struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	Tracked     bool   `json:"tracked"`
	MatchesHead bool   `json:"matches_head"`
	SHA256      string `json:"sha256,omitempty"`
}

type EnvironmentRequirement struct {
	Value      string `json:"value"`
	Mode       string `json:"mode"`
	DeclaredBy string `json:"declared_by"`
	Ownership  string `json:"ownership"`
}

type EnvironmentTool struct {
	Name          string                   `json:"name"`
	Executable    string                   `json:"executable"`
	Role          string                   `json:"role"`
	Required      bool                     `json:"required"`
	Available     bool                     `json:"available"`
	Path          string                   `json:"path,omitempty"`
	Version       string                   `json:"version,omitempty"`
	Source        string                   `json:"source,omitempty"`
	Compatibility string                   `json:"compatibility"`
	Reason        string                   `json:"reason,omitempty"`
	Requirements  []EnvironmentRequirement `json:"requirements,omitempty"`
}

type EnvironmentLanguage struct {
	Name       string   `json:"name"`
	DeclaredBy []string `json:"declared_by"`
}

type EnvironmentMechanism struct {
	Name       string   `json:"name"`
	DeclaredBy []string `json:"declared_by"`
	Tool       string   `json:"tool,omitempty"`
	Available  bool     `json:"available"`
	Version    string   `json:"version,omitempty"`
	Notes      string   `json:"notes,omitempty"`
}

type EnvironmentEntrypoint struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	File    string `json:"file"`
	Command string `json:"command,omitempty"`
}

type RepositoryEnvironmentSummary struct {
	SchemaVersion        int                      `json:"schema_version"`
	RepositoryRoot       string                   `json:"repository_root"`
	RepositoryHead       string                   `json:"repository_head"`
	Declarations         []EnvironmentDeclaration `json:"declarations,omitempty"`
	Languages            []EnvironmentLanguage    `json:"languages,omitempty"`
	Mechanisms           []EnvironmentMechanism   `json:"mechanisms,omitempty"`
	Tools                []EnvironmentTool        `json:"tools,omitempty"`
	Entrypoints          []EnvironmentEntrypoint  `json:"entrypoints,omitempty"`
	HostStatus           string                   `json:"host_status"`
	SelectionRequired    bool                     `json:"selection_required,omitempty"`
	SelectionReason      string                   `json:"selection_reason,omitempty"`
	IsolationDeclared    bool                     `json:"isolation_declared,omitempty"`
	IsolationRequirement string                   `json:"isolation_requirement,omitempty"`
	IsolationMechanisms  []string                 `json:"isolation_mechanisms,omitempty"`
	Warnings             []string                 `json:"warnings,omitempty"`
}
