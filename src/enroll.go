package main

// Pre-registration: the account page creates an already approved binding and
// hands the user an enrollment file holding a one-time token. The Connector
// imports it (serve --enroll, install --enroll, or enrollment.json dropped in
// the state directory) and claims its machine identity with the token as the
// binding proof; no pairing code or account password reaches this machine.
import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	enrollmentFile   = "enrollment.json"
	enrollmentType   = "openplayx-connector-enrollment"
	enrollmentMaxLen = 4096
)

var enrollmentTokenPattern = regexp.MustCompile(`^opxe_[A-Za-z0-9_-]{43}$`)

type connectorEnrollment struct {
	Schema    int       `json:"schemaVersion"`
	Type      string    `json:"type"`
	API       string    `json:"api,omitempty"`
	Name      string    `json:"name"`
	Token     string    `json:"enrollmentToken"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// parseEnrollment validates an enrollment file. origin is the platform API the
// Connector will talk to; a file issued for another origin is refused rather
// than silently claimed elsewhere.
func parseEnrollment(raw []byte, origin string, at time.Time) (connectorEnrollment, error) {
	var e connectorEnrollment
	if len(raw) > enrollmentMaxLen {
		return e, errors.New("invalid_enrollment")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if decoder.Decode(&e) != nil || e.Schema != 1 || e.Type != enrollmentType {
		return e, errors.New("invalid_enrollment")
	}
	if e.API != "" && strings.TrimRight(e.API, "/") != origin {
		return e, errors.New("enrollment_origin_mismatch")
	}
	e.Name = strings.TrimSpace(e.Name)
	if !enrollmentTokenPattern.MatchString(e.Token) || e.Name == "" || len(e.Name) > 128 || e.ExpiresAt.IsZero() {
		return e, errors.New("invalid_enrollment")
	}
	if !at.Before(e.ExpiresAt) {
		return e, errors.New("enrollment_expired")
	}
	return e, nil
}

// importEnrollment replaces any pending pairing code with the pre-approved
// token; the next sync claims the machine identity.
func (p *connectorPlatform) importEnrollment(raw []byte) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state.MachineToken != "" {
		return "", errors.New("connector_already_bound")
	}
	e, err := parseEnrollment(raw, p.origin, time.Now())
	if err != nil {
		return "", err
	}
	p.state.Proof = e.Token
	p.state.Code = ""
	p.state.Enrollment = e.Name
	p.state.ExpiresAt = e.ExpiresAt
	return e.Name, p.save()
}

// pickupEnrollment imports enrollment.json from the state directory (placed by
// install --enroll) and always removes it: the token then lives only in the
// private platform state.
func (p *connectorPlatform) pickupEnrollment() {
	path := p.store.file(enrollmentFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	name, err := p.importEnrollment(raw)
	if err != nil {
		log.Printf("enrollment file not imported: %v", err)
	} else {
		fmt.Printf("Pre-registration %q imported; claiming the platform identity.\n", name)
	}
	if err := os.Remove(path); err != nil {
		log.Printf("remove %s: %v", path, err)
	}
}

// stageEnrollment validates an enrollment file and copies it into the state
// directory for the service to pick up on start.
// A Connector that is already bound ignores the file, so re-running the same
// command after the token expired still starts the service.
func stageEnrollment(source string, store *Store) error {
	if platformBound(store) {
		fmt.Println("Connector is already bound to a platform account; --enroll ignored.")
		return nil
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read enrollment file: %w", err)
	}
	if _, err = parseEnrollment(raw, platformOrigin, time.Now()); err != nil {
		return err
	}
	return os.WriteFile(store.file(enrollmentFile), raw, 0o600)
}

func platformBound(store *Store) bool {
	raw, err := os.ReadFile(store.file("platform.json"))
	if err != nil {
		return false
	}
	var state platformState
	return json.Unmarshal(raw, &state) == nil && state.MachineToken != ""
}
