// Package development owns durable developer workspaces and package-level Git
// activation.
package development

import "time"

type State string

const (
	StateCreating   State = "CREATING"
	StateStarting   State = "STARTING"
	StateReady      State = "READY"
	StateBusy       State = "BUSY"
	StateActivating State = "ACTIVATING"
	StateConflicted State = "CONFLICTED"
	StateStopping   State = "STOPPING"
	StateStopped    State = "STOPPED"
	StateFailed     State = "FAILED"
	StateResetting  State = "RESETTING"
	StateDeleting   State = "DELETING"
)

type MountBehavior string

const (
	MountWorkspaceSource MountBehavior = "workspace-source"
	MountReadOnly        MountBehavior = "read-only-shared"
	MountPersistent      MountBehavior = "persistent-user"
	MountEphemeral       MountBehavior = "ephemeral"
)

type MountDefinition struct {
	ID                   string        `toml:"id" json:"mount_id"`
	Source               string        `toml:"source" json:"source"`
	Target               string        `toml:"target" json:"target"`
	Behavior             MountBehavior `toml:"behavior" json:"behavior"`
	Writable             bool          `toml:"writable" json:"writable"`
	Persistence          string        `toml:"persistence" json:"persistence"`
	ParticipatesActivate bool          `toml:"participates_activation" json:"participates_activation"`
	WorkspaceOwned       bool          `toml:"workspace_owned" json:"workspace_owned"`
}

type Workspace struct {
	Schema               int               `toml:"schema" json:"schema"`
	WorkspaceID          string            `toml:"workspace_id" json:"workspace_id"`
	OwnerUserID          string            `toml:"owner_user_id" json:"owner_user_id"`
	ActiveSandboxID      string            `toml:"active_sandbox_id,omitempty" json:"active_sandbox_id,omitempty"`
	DevelopmentImage     string            `toml:"development_image_digest,omitempty" json:"development_image_digest,omitempty"`
	SourcePath           string            `toml:"source_path" json:"-"`
	SystemPath           string            `toml:"system_path,omitempty" json:"-"`
	MountProfile         []MountDefinition `toml:"mounts" json:"mount_profile"`
	State                State             `toml:"state" json:"state"`
	WritesPaused         bool              `toml:"writes_paused" json:"writes_paused"`
	ActivationActive     bool              `toml:"activation_active" json:"activation_active"`
	CanSafelyReset       bool              `toml:"-" json:"can_safely_reset"`
	ConflictedPackages   []string          `toml:"conflicted_packages,omitempty" json:"conflicted_packages,omitempty"`
	LastActivationStatus string            `toml:"last_activation_status,omitempty" json:"last_activation_status,omitempty"`
	LastActivationAt     time.Time         `toml:"last_activation_at" json:"last_activation_at,omitempty"`
	LastActivationResult *ActivationResult `toml:"last_activation_result,omitempty" json:"last_activation_result,omitempty"`
	CreatedAt            time.Time         `toml:"created_at" json:"created_at"`
	UpdatedAt            time.Time         `toml:"updated_at" json:"updated_at"`
	Token                string            `toml:"workspace_token" json:"-"`
}

type Repository struct {
	PackageID       string `json:"package_id"`
	Path            string `json:"path"`
	ActivationReady bool   `json:"activation_ready"`
	Branch          string `json:"branch,omitempty"`
	Head            string `json:"head,omitempty"`
	RemoteName      string `json:"remote_name,omitempty"`
	RemoteURL       string `json:"remote_url,omitempty"`
	Clean           bool   `json:"clean"`
	Status          string `json:"status"`
}

type ImageStatus struct {
	Digest       string    `json:"digest,omitempty"`
	BuiltAt      time.Time `json:"built_at,omitempty"`
	CodexVersion string    `json:"codex_version,omitempty"`
	DenoVersion  string    `json:"deno_version,omitempty"`
	BuildStatus  string    `json:"build_status"`
}

type ActivationOptions struct {
	Description      string            `json:"description"`
	SelectedPackages []string          `json:"selected_packages,omitempty"`
	PackageMessages  map[string]string `json:"package_messages,omitempty"`
	AuthorName       string            `json:"author_name,omitempty"`
	AuthorEmail      string            `json:"author_email,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type ActivationFile struct {
	Path   string `json:"path"`
	Change string `json:"change"`
}

type ActivationPackagePreview struct {
	PackageID       string           `json:"package_id"`
	Selected        bool             `json:"selected"`
	BaseCommit      string           `json:"base_commit,omitempty"`
	SharedCommit    string           `json:"current_shared_commit,omitempty"`
	RequiresMerge   bool             `json:"requires_merge"`
	ActivationReady bool             `json:"activation_ready"`
	RemoteName      string           `json:"remote_name,omitempty"`
	RemoteURL       string           `json:"remote_url,omitempty"`
	Files           []ActivationFile `json:"files"`
}

type ActivationPreview struct {
	WorkspaceID string                     `json:"workspace_id"`
	Packages    []ActivationPackagePreview `json:"packages"`
}

type ActivationPackageResult struct {
	PackageID     string   `json:"package_id"`
	Status        string   `json:"status"`
	PreviousHead  string   `json:"previous_commit,omitempty"`
	ResultingHead string   `json:"resulting_commit,omitempty"`
	CommitMessage string   `json:"commit_message,omitempty"`
	Conflicts     []string `json:"conflicts,omitempty"`
	Error         string   `json:"error,omitempty"`
}

type ActivationResult struct {
	WorkspaceID string                    `json:"workspace_id"`
	Success     bool                      `json:"success"`
	Status      string                    `json:"status"`
	Packages    []ActivationPackageResult `json:"packages"`
}

// MountProfileDocument is the shared, operator-editable development mount
// profile stored outside package repositories and sandbox-visible paths.
type MountProfileDocument struct {
	Schema int               `toml:"schema" json:"schema"`
	Mounts []MountDefinition `toml:"mounts" json:"mounts"`
}

type ShellResult struct {
	WorkspaceID string `json:"workspace_id"`
	SandboxID   string `json:"sandbox_id"`
	Command     string `json:"command"`
	Output      string `json:"output"`
}
