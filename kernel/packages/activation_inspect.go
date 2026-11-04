package packages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	idgen "the8020/kernel/identity"
)

// ActivationInspection is a point-in-time record, not proof that work is settled.
type ActivationInspection struct {
	ID         string                 `json:"id"`
	Stage      string                 `json:"stage"`
	Error      string                 `json:"error,omitempty"`
	Candidates []ActivationMemberInfo `json:"candidates"`
}

type ActivationMemberInfo struct {
	PackageID      string               `json:"package_id"`
	PreviousCommit string               `json:"previous_commit,omitempty"`
	Commit         string               `json:"commit"`
	Hooks          []ActivationHookInfo `json:"hooks"`
}

type ActivationHookInfo struct {
	Hook     string `json:"hook"`
	State    string `json:"state"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error,omitempty"`
}

// InspectActivation reads the exact native record without resuming its owner.
func (s *Store) InspectActivation(ctx context.Context, id string) (ActivationInspection, error) {
	owner, ok := s.schemaDeployment().(interface {
		InspectActivation(context.Context, string) (ActivationInspection, error)
	})
	if !ok {
		return ActivationInspection{}, errors.New("activation inspection is unavailable")
	}
	return owner.InspectActivation(ctx, id)
}

func (c *ActivationCoordinator) InspectActivation(ctx context.Context, id string) (ActivationInspection, error) {
	if !idgen.Is(id, "act") {
		return ActivationInspection{}, errors.New("invalid activation identity")
	}
	rows, err := c.database.QueryContext(ctx, `SELECT a."stage", SUBSTR(a."error", 1, 2048), p."packageId", p."previousCommit", p."candidateCommit", h."hook", h."state", h."attempts", SUBSTR(h."error", 1, 2048)
		FROM `+activationsTable+` a LEFT JOIN `+activationPackagesTable+` p ON p."activationId" = a."activationId"
		LEFT JOIN `+hookRunsTable+` h ON h."activationId" = p."activationId" AND h."packageId" = p."packageId"
		WHERE a."activationId" = $1 ORDER BY p."packageId", h."hook" LIMIT 513`, id)
	if err != nil {
		return ActivationInspection{}, err
	}
	defer rows.Close()
	result := ActivationInspection{ID: id, Candidates: []ActivationMemberInfo{}}
	count := 0
	for rows.Next() {
		count++
		if count > 512 {
			return ActivationInspection{}, errors.New("activation inspection exceeds 512 hook records")
		}
		var stage string
		var failure, packageID, previous, commit, hook, state, hookFailure sql.NullString
		var attempts sql.NullInt64
		if err := rows.Scan(&stage, &failure, &packageID, &previous, &commit, &hook, &state, &attempts, &hookFailure); err != nil {
			return ActivationInspection{}, err
		}
		result.Stage, result.Error = stage, failure.String
		if !packageID.Valid {
			continue
		}
		if len(result.Candidates) == 0 || result.Candidates[len(result.Candidates)-1].PackageID != packageID.String {
			result.Candidates = append(result.Candidates, ActivationMemberInfo{PackageID: packageID.String, PreviousCommit: previous.String, Commit: commit.String, Hooks: []ActivationHookInfo{}})
		}
		if hook.Valid {
			member := &result.Candidates[len(result.Candidates)-1]
			member.Hooks = append(member.Hooks, ActivationHookInfo{Hook: hook.String, State: state.String, Attempts: int(attempts.Int64), Error: hookFailure.String})
		}
	}
	if err := rows.Err(); err != nil {
		return ActivationInspection{}, err
	}
	if count == 0 {
		return ActivationInspection{}, fmt.Errorf("activation %s not found", id)
	}
	return result, nil
}
