package kata

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kata/internal/db"
	katauid "go.kenn.io/kata/internal/uid"
)

// ErrFederationEnrollmentNotFound reports that an enrollment does not belong
// to the requested project or does not exist.
var ErrFederationEnrollmentNotFound = errors.New("kata: federation enrollment not found")

// FederationEnrollmentSpec describes a project-scoped transport credential.
// Kata generates the plaintext token and returns it only from creation.
type FederationEnrollmentSpec struct {
	ProjectUID                   string
	SpokeInstanceUID             string
	Capabilities                 string
	Actor                        string
	AllowAdoptionSnapshotAuthors bool
}

// FederationEnrollment is the host-visible enrollment history. It omits the
// stored token hash and never contains the plaintext credential.
type FederationEnrollment struct {
	ID                           int64
	ProjectUID                   string
	SpokeInstanceUID             string
	Capabilities                 string
	Actor                        string
	AllowAdoptionSnapshotAuthors bool
	CreatedAt                    time.Time
	UpdatedAt                    time.Time
	RevokedAt                    *time.Time
}

// CreatedFederationEnrollment contains a new enrollment and its one-time
// plaintext credential.
type CreatedFederationEnrollment struct {
	Enrollment FederationEnrollment
	Token      string
}

// CreateFederationEnrollment enables hub federation for the project when
// necessary and creates a project-scoped transport credential. It is an
// in-process application method; the caller must authorize the operation.
func (s *Service) CreateFederationEnrollment(
	ctx context.Context,
	spec FederationEnrollmentSpec,
) (CreatedFederationEnrollment, error) {
	capabilities, actor, err := validateFederationEnrollmentSpec(spec)
	if err != nil {
		return CreatedFederationEnrollment{}, err
	}
	callCtx, done, err := s.beginHostCall(ctx)
	if err != nil {
		return CreatedFederationEnrollment{}, err
	}
	defer done()

	project, found, err := s.projectByUID(callCtx, spec.ProjectUID)
	if err != nil {
		return CreatedFederationEnrollment{}, err
	}
	if !found || project.DeletedAt != nil {
		return CreatedFederationEnrollment{}, ErrProjectNotFound
	}
	created, err := s.store.CreateProjectFederationEnrollment(callCtx, db.CreateFederationEnrollmentParams{
		SpokeInstanceUID:             spec.SpokeInstanceUID,
		ProjectID:                    &project.ID,
		Capabilities:                 capabilities,
		Actor:                        actor,
		AllowAdoptionSnapshotAuthors: spec.AllowAdoptionSnapshotAuthors,
	})
	if errors.Is(err, db.ErrNotFound) {
		return CreatedFederationEnrollment{}, ErrProjectNotFound
	}
	if err != nil {
		return CreatedFederationEnrollment{}, fmt.Errorf("kata: create federation enrollment: %w", err)
	}
	return CreatedFederationEnrollment{
		Enrollment: publicFederationEnrollment(created.Enrollment, project.UID),
		Token:      created.Token,
	}, nil
}

// ListFederationEnrollments returns retained enrollment history for one stable
// project identity. Plaintext credentials and their stored hashes are omitted.
func (s *Service) ListFederationEnrollments(
	ctx context.Context,
	projectUID string,
) ([]FederationEnrollment, error) {
	if !validHostProjectUID(projectUID) {
		return nil, ErrProjectNotFound
	}
	callCtx, done, err := s.beginHostCall(ctx)
	if err != nil {
		return nil, err
	}
	defer done()

	project, found, err := s.projectByUID(callCtx, projectUID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrProjectNotFound
	}
	enrollments, err := s.store.ListProjectFederationEnrollments(callCtx, project.ID)
	if err != nil {
		return nil, fmt.Errorf("kata: list federation enrollments: %w", err)
	}
	result := make([]FederationEnrollment, 0, len(enrollments))
	for _, enrollment := range enrollments {
		result = append(result, publicFederationEnrollment(enrollment, project.UID))
	}
	return result, nil
}

// FindActiveFederationEnrollment finds an active project credential by its
// public correlation fields without scanning retained enrollment history.
// Plaintext credentials and their stored hashes are never returned.
func (s *Service) FindActiveFederationEnrollment(
	ctx context.Context,
	spec FederationEnrollmentSpec,
) (FederationEnrollment, bool, error) {
	capabilities, actor, err := validateFederationEnrollmentSpec(spec)
	if err != nil {
		return FederationEnrollment{}, false, err
	}
	callCtx, done, err := s.beginHostCall(ctx)
	if err != nil {
		return FederationEnrollment{}, false, err
	}
	defer done()

	project, found, err := s.projectByUID(callCtx, spec.ProjectUID)
	if err != nil {
		return FederationEnrollment{}, false, err
	}
	if !found {
		return FederationEnrollment{}, false, ErrProjectNotFound
	}
	enrollment, err := s.store.FindActiveFederationEnrollment(
		callCtx,
		db.ActiveFederationEnrollmentParams{
			ProjectID:                    project.ID,
			SpokeInstanceUID:             spec.SpokeInstanceUID,
			Capabilities:                 capabilities,
			Actor:                        actor,
			AllowAdoptionSnapshotAuthors: spec.AllowAdoptionSnapshotAuthors,
		},
	)
	if errors.Is(err, db.ErrNotFound) {
		return FederationEnrollment{}, false, nil
	}
	if err != nil {
		return FederationEnrollment{}, false, fmt.Errorf(
			"kata: find active federation enrollment: %w",
			err,
		)
	}
	return publicFederationEnrollment(enrollment, project.UID), true, nil
}

// RevokeFederationEnrollment permanently revokes one credential belonging to
// the requested project. Repeating an exact revocation is harmless.
func (s *Service) RevokeFederationEnrollment(
	ctx context.Context,
	projectUID string,
	enrollmentID int64,
) error {
	if !validHostProjectUID(projectUID) || enrollmentID <= 0 {
		return ErrFederationEnrollmentNotFound
	}
	callCtx, done, err := s.beginHostCall(ctx)
	if err != nil {
		return err
	}
	defer done()

	project, found, err := s.projectByUID(callCtx, projectUID)
	if err != nil {
		return err
	}
	if !found {
		return ErrProjectNotFound
	}
	enrollment, found, err := s.projectFederationEnrollment(callCtx, project.ID, enrollmentID)
	if err != nil {
		return fmt.Errorf("kata: find federation enrollment: %w", err)
	}
	if !found {
		// Absent, or owned by a different project: the same answer either
		// way, so a caller cannot probe another project's enrollment IDs.
		return ErrFederationEnrollmentNotFound
	}
	if enrollment.RevokedAt != nil {
		return nil
	}
	if err := s.store.RevokeFederationEnrollment(callCtx, enrollmentID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return ErrFederationEnrollmentNotFound
		}
		return fmt.Errorf("kata: revoke federation enrollment: %w", err)
	}
	return nil
}

func validateFederationEnrollmentSpec(spec FederationEnrollmentSpec) (string, string, error) {
	if !validHostProjectUID(spec.ProjectUID) {
		return "", "", ErrProjectNotFound
	}
	if !katauid.Valid(spec.SpokeInstanceUID) {
		return "", "", errors.New("kata: invalid spoke instance UID")
	}
	capabilities, err := db.CanonicalFederationCapabilities(spec.Capabilities)
	if err != nil {
		return "", "", fmt.Errorf("kata: invalid federation capabilities: %w", err)
	}
	actor := strings.TrimSpace(spec.Actor)
	if err := db.ValidateTokenActor(actor); err != nil {
		return "", "", fmt.Errorf("kata: invalid federation enrollment actor: %w", err)
	}
	return capabilities, actor, nil
}

func validHostProjectUID(projectUID string) bool {
	return katauid.Valid(projectUID) && projectUID != db.SystemProjectUID
}

// projectFederationEnrollment finds one enrollment inside a project's own
// scope. Ownership is a database predicate rather than a Go-side filter, so
// "absent" and "belongs to another project" collapse into one answer.
func (s *Service) projectFederationEnrollment(
	ctx context.Context,
	projectID int64,
	enrollmentID int64,
) (db.FederationEnrollment, bool, error) {
	enrollments, err := s.store.ListProjectFederationEnrollments(ctx, projectID)
	if err != nil {
		return db.FederationEnrollment{}, false, err
	}
	for _, enrollment := range enrollments {
		if enrollment.ID == enrollmentID {
			return enrollment, true, nil
		}
	}
	return db.FederationEnrollment{}, false, nil
}

func publicFederationEnrollment(
	enrollment db.FederationEnrollment,
	projectUID string,
) FederationEnrollment {
	return FederationEnrollment{
		ID:                           enrollment.ID,
		ProjectUID:                   projectUID,
		SpokeInstanceUID:             enrollment.SpokeInstanceUID,
		Capabilities:                 enrollment.Capabilities,
		Actor:                        enrollment.Actor,
		AllowAdoptionSnapshotAuthors: enrollment.AllowAdoptionSnapshotAuthors,
		CreatedAt:                    enrollment.CreatedAt,
		UpdatedAt:                    enrollment.UpdatedAt,
		RevokedAt:                    enrollment.RevokedAt,
	}
}
