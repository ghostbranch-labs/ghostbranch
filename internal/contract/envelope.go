package contract

// ErrorBody is the serialized form of an Error.
type ErrorBody struct {
	Code      Code           `json:"code"`
	Message   string         `json:"message"`
	Field     *string        `json:"field"`
	Retryable bool           `json:"retryable"`
	Details   map[string]any `json:"details"`
}

// Next is a command the caller can run now, exactly as given.
type Next struct {
	Command    []string `json:"command"`
	Why        string   `json:"why"`
	NeedsHuman bool     `json:"needs_human"`
	NeedsInput []string `json:"needs_input"`
}

// Envelope is the one JSON object a command prints with --json.
type Envelope struct {
	SchemaVersion string     `json:"schema_version"`
	Command       string     `json:"command"`
	OK            bool       `json:"ok"`
	ExitCode      int        `json:"exit_code"`
	Data          any        `json:"data"`
	Warnings      []Warning  `json:"warnings"`
	Next          []Next     `json:"next"`
	Error         *ErrorBody `json:"error"`
}

// NewEnvelope builds the output for a finished command. data may be nil.
func NewEnvelope(command string, data any, warnings []Warning, next []Next, err error) Envelope {
	if warnings == nil {
		warnings = []Warning{}
	}
	if next == nil {
		next = []Next{}
	}
	for i := range next {
		if next[i].NeedsInput == nil {
			next[i].NeedsInput = []string{}
		}
	}
	if data == nil {
		data = map[string]any{}
	}
	env := Envelope{
		SchemaVersion: SchemaVersion,
		Command:       command,
		OK:            err == nil,
		ExitCode:      ExitOK,
		Data:          data,
		Warnings:      warnings,
		Next:          next,
	}
	if ce := From(err); ce != nil {
		env.ExitCode = ce.Code.Exit()
		body := &ErrorBody{Code: ce.Code, Message: ce.Message, Retryable: ce.Retryable, Details: ce.Details}
		if ce.Field != "" {
			f := ce.Field
			body.Field = &f
		}
		if body.Details == nil {
			body.Details = map[string]any{}
		}
		env.Error = body
	}
	return env
}
