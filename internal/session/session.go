package session

import (
	"fmt"
	"pcap_agent/internal/common"
	"pcap_agent/internal/events"
	"time"
)

// Session manages a single analysis session with multi-round conversations.
type Session struct {
	ID       string
	PcapPath string
	RoundNum int
	store    *Store
	emitter  events.Emitter
}

// NewSession creates a new session and persists it to the store.
func NewSession(store *Store, emitter events.Emitter, pcapPath string) (*Session, error) {
	id := fmt.Sprintf("sess_%d", time.Now().UnixMilli())
	if err := store.CreateSession(id, pcapPath); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &Session{
		ID:       id,
		PcapPath: pcapPath,
		RoundNum: 0,
		store:    store,
		emitter:  emitter,
	}, nil
}

// ResumeSession loads an existing session from the store.
func ResumeSession(store *Store, emitter events.Emitter, sessionID string) (*Session, error) {
	exists, err := store.SessionExists(sessionID)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	pcapPath, err := store.GetSessionPcapPath(sessionID)
	if err != nil {
		return nil, err
	}
	roundCount, err := store.GetRoundCount(sessionID)
	if err != nil {
		return nil, err
	}
	return &Session{
		ID:       sessionID,
		PcapPath: pcapPath,
		RoundNum: roundCount,
		store:    store,
		emitter:  emitter,
	}, nil
}

// History loads accumulated context from all previous rounds.
// Returns nil if this is the first round.
func (s *Session) History() (*common.SessionHistory, error) {
	if s.RoundNum == 0 {
		return nil, nil
	}
	return s.store.GetSessionHistory(s.ID)
}

// SaveRound persists a completed round (planner + all executor steps + report).
func (s *Session) SaveRound(userQuery string, plan common.Plan, report, findings, opLog string) error {
	s.RoundNum++
	roundID, err := s.store.SaveRound(s.ID, s.RoundNum, userQuery, plan, report, findings, opLog)
	if err != nil {
		return fmt.Errorf("save round: %w", err)
	}

	// Save individual steps
	for _, step := range plan.Steps {
		if err := s.store.SaveStep(roundID, step.StepID, step.Intent, "", "", "completed"); err != nil {
			return fmt.Errorf("save step %d: %w", step.StepID, err)
		}
	}

	if err := s.store.TouchSession(s.ID); err != nil {
		return fmt.Errorf("touch session: %w", err)
	}

	return nil
}
