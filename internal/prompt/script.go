package prompt

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// CtrlC as a scripted answer is a Ctrl-C at that question.
const CtrlC = "\x03"

var ErrNoAnswers = errors.New("prompt: the script has no answer left")

// Script is an Asker for tests. It answers from a queue with the console's
// rules (empty is the default, three unclear yes/no answers fail) and
// records what was shown.
type Script struct {
	mu      sync.Mutex
	answers []string
	Asked   []string // every question as shown, re-asks included
	Said    []string
	missed  []string // questions asked after the answers ran out
}

func NewScript(answers ...string) *Script { return &Script{answers: answers} }

func (s *Script) ask(ctx context.Context, text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Asked = append(s.Asked, text)
	if ctx.Err() != nil {
		return "", interrupted(ctx, text)
	}
	if len(s.answers) == 0 {
		s.missed = append(s.missed, text)
		return "", fmt.Errorf("%w: %q", ErrNoAnswers, text)
	}
	ans := s.answers[0]
	s.answers = s.answers[1:]
	if ans == CtrlC {
		return "", contract.New(contract.Interrupted, "interrupted (Ctrl-C) at: %s", text)
	}
	return ans, nil
}

func (s *Script) Say(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Said = append(s.Said, message)
}

func (s *Script) YesNo(ctx context.Context, q string, def bool) (bool, error) {
	return yesNo(ctx, q, def, s.ask, s.Say)
}

func (s *Script) Text(ctx context.Context, q, def string) (string, error) {
	return text(ctx, q, def, s.ask)
}

func (s *Script) Wait(ctx context.Context, message string) error {
	_, err := s.ask(ctx, message+" ")
	return err
}

// Check fails t when a question found no answer or answers were left over.
func (s *Script) Check(t interface {
	Helper()
	Errorf(string, ...any)
}) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.missed) > 0 {
		t.Errorf("prompt script ran out of answers at %q (asked so far: %q)", s.missed, s.Asked)
	}
	if len(s.answers) > 0 {
		t.Errorf("prompt script has %d unused answer(s): %q", len(s.answers), s.answers)
	}
}
