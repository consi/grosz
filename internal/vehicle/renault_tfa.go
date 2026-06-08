package vehicle

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/consi/grosz/internal/events"
)

// ErrTFANotRequired is returned by StartTFA when accounts.login succeeds
// without a TFA challenge — the account either has TFA disabled or this device
// is still trusted, so no email code is needed.
var ErrTFANotRequired = errors.New("renault: TFA not required")

// tfaPendingTTL bounds how long a started verification (emailed code) stays
// valid before the user must request a fresh code.
const tfaPendingTTL = 10 * time.Minute

// tfaPending holds the short-lived secrets of an in-flight email-TFA
// verification, carried between StartTFA (sends the code) and CompleteTFA
// (submits it). These never touch disk — only the resulting login_token does.
type tfaPending struct {
	regToken       string
	gigyaAssertion string
	phvToken       string
	obfuscated     string
	gmid           string
	ucid           string
	user           string
	pass           string
	createdAt      time.Time
}

// TFAState is the UI-facing snapshot of the Renault TFA situation.
type TFAState struct {
	Configured      bool   `json:"configured"`
	Required        bool   `json:"required"`
	Pending         bool   `json:"pending"`
	ObfuscatedEmail string `json:"obfuscatedEmail,omitempty"`
	CompletedAt     string `json:"completedAt,omitempty"`
}

// TFAStatus returns the current verification state for the settings UI.
func (r *Renault) TFAStatus() TFAState {
	r.mu.RLock()
	pending := r.tfa
	r.mu.RUnlock()

	st := TFAState{
		Configured: r.store.GetDefault("vehicle.renault_user", "") != "" &&
			r.store.GetDefault("vehicle.renault_password", "") != "",
		Required:    r.store.GetBool("vehicle.renault_tfa_required", false),
		CompletedAt: r.store.GetDefault("vehicle.renault_tfa_completed_at", ""),
	}
	if pending != nil && time.Since(pending.createdAt) <= tfaPendingTTL {
		st.Pending = true
		st.ObfuscatedEmail = pending.obfuscated
	}
	return st
}

// StartTFA begins the email two-factor flow: it logs in (expecting a 403101
// challenge), bootstraps a trusted-device context, and triggers an emailed
// verification code. On success it returns the obfuscated destination email and
// stashes the in-flight secrets for CompleteTFA. If login succeeds outright it
// persists the session and returns ErrTFANotRequired.
func (r *Renault) StartTFA(user, pass string) (string, error) {
	if user == "" || pass == "" {
		return "", fmt.Errorf("renault credentials not configured")
	}

	// Establish a fresh device context first so finalizeTFA(tempDevice=false)
	// can mark THIS device trusted for ~30 days. Best-effort: the rest of the
	// flow still works via regToken if the bootstrap call fails.
	if gmid, ucid, err := r.bootstrapDevice(); err != nil {
		r.log.Warn("renault tfa: device bootstrap failed, continuing without gmid", "err", err)
	} else {
		r.mu.Lock()
		if gmid != "" {
			r.gmid = gmid
		}
		if ucid != "" {
			r.ucid = ucid
		}
		r.mu.Unlock()
	}

	// We expect login to fail with 403101 (pending TFA) and carry a regToken.
	session, err := r.gigyaLogin(user, pass)
	if err == nil {
		// TFA not enforced (or device already trusted) — just persist & poll.
		r.mu.Lock()
		r.session = session
		r.jwt = ""
		r.jwtExpiry = time.Time{}
		r.mu.Unlock()
		_ = r.store.Set("vehicle.renault_session", session)
		r.persistDevice()
		_ = r.store.Set("vehicle.renault_tfa_required", "false")
		r.Trigger()
		return "", ErrTFANotRequired
	}
	var ge *gigyaError
	if !errors.As(err, &ge) || ge.code != gigyaErrPendingTFA {
		return "", err
	}
	regToken := ge.regToken
	if regToken == "" {
		return "", fmt.Errorf("renault tfa: 403101 response carried no regToken")
	}

	assertion, err := r.initTFA(regToken)
	if err != nil {
		return "", err
	}
	emailID, obfuscated, err := r.getTFAEmails(assertion)
	if err != nil {
		return "", err
	}
	phvToken, err := r.sendTFACode(emailID, assertion)
	if err != nil {
		return "", err
	}

	r.mu.RLock()
	gmid, ucid := r.gmid, r.ucid
	r.mu.RUnlock()

	r.mu.Lock()
	r.tfa = &tfaPending{
		regToken:       regToken,
		gigyaAssertion: assertion,
		phvToken:       phvToken,
		obfuscated:     obfuscated,
		gmid:           gmid,
		ucid:           ucid,
		user:           user,
		pass:           pass,
		createdAt:      time.Now(),
	}
	r.mu.Unlock()

	_ = r.store.Set("vehicle.renault_tfa_required", "true")
	r.events.Info(events.ActionRenaultTFA,
		map[string]any{"step": "codeSent"},
		map[string]any{"email": obfuscated},
	)
	return obfuscated, nil
}

// CompleteTFA submits the emailed code, finalizes the verification as a trusted
// device, and persists the resulting login_token. The next poll uses it.
func (r *Renault) CompleteTFA(code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return fmt.Errorf("verification code required")
	}

	r.mu.RLock()
	pending := r.tfa
	r.mu.RUnlock()
	if pending == nil {
		return fmt.Errorf("no TFA verification in progress; request a new code")
	}
	if time.Since(pending.createdAt) > tfaPendingTTL {
		r.mu.Lock()
		r.tfa = nil
		r.mu.Unlock()
		return fmt.Errorf("verification expired; request a new code")
	}

	// The gigyaAssertion minted by initTFA is a short-lived JWT (~5 min). If the
	// user is slow, it expires mid-flow and Gigya rejects the call with 400006.
	// Record elapsed time alongside the failing step so we can tell an expiry
	// apart from a genuinely wrong code (errorDetails names the bad parameter).
	elapsed := time.Since(pending.createdAt)
	recordFail := func(step string, err error) error {
		r.events.Warn(events.ActionRenaultTFA,
			map[string]any{"step": step},
			map[string]any{"error": err.Error(), "elapsedSec": int(elapsed.Seconds())},
		)
		return err
	}

	providerAssertion, err := r.completeTFAVerification(pending.gigyaAssertion, pending.phvToken, code)
	if err != nil {
		return recordFail("completeVerification", err)
	}
	// tempDevice=false → Gigya remembers this device for ~30 days, so routine
	// re-logins (e.g. after the login_token expires) skip TFA entirely.
	if err := r.finalizeTFA(pending.gigyaAssertion, providerAssertion, pending.regToken, false); err != nil {
		return recordFail("finalizeTFA", err)
	}

	// finalizeTFA has trusted this device. Lock in the gmid/ucid and persist
	// them immediately: from here the trust is real, and it must survive even if
	// the session-minting login below hiccups or the process restarts (the next
	// poll's login then presents the gmid and skips TFA on its own).
	r.mu.Lock()
	if pending.gmid != "" {
		r.gmid = pending.gmid
	}
	if pending.ucid != "" {
		r.ucid = pending.ucid
	}
	r.mu.Unlock()
	r.persistDevice()

	// Mint the durable session with a fresh login that presents the now-trusted
	// gmid — the same path the poller uses to skip TFA. We deliberately do NOT
	// call accounts.finalizeRegistration: it carries no device context, so Gigya
	// still treats the regToken as TFA-pending and answers 403101.
	session, err := r.gigyaLogin(pending.user, pending.pass)
	if err != nil {
		return recordFail("login", err)
	}

	r.mu.Lock()
	r.session = session
	r.jwt = "" // force a fresh JWT minted from the new session
	r.jwtExpiry = time.Time{}
	r.tfa = nil
	r.mu.Unlock()

	_ = r.store.Set("vehicle.renault_session", session)
	_ = r.store.Set("vehicle.renault_tfa_completed_at", time.Now().UTC().Format(time.RFC3339))
	_ = r.store.Set("vehicle.renault_tfa_required", "false")

	r.events.Info(events.ActionRenaultTFA,
		map[string]any{"step": "completed"},
		map[string]any{"trustedDevice": true},
	)
	r.Trigger()
	return nil
}

// addDevice attaches the trusted-device identifiers to a Gigya form when known.
func (r *Renault) addDevice(form url.Values) {
	r.mu.RLock()
	gmid, ucid := r.gmid, r.ucid
	r.mu.RUnlock()
	if gmid != "" {
		form.Set("gmid", gmid)
	}
	if ucid != "" {
		form.Set("ucid", ucid)
	}
}

// persistDevice writes the current device identifiers to the store.
func (r *Renault) persistDevice() {
	r.mu.RLock()
	gmid, ucid := r.gmid, r.ucid
	r.mu.RUnlock()
	if gmid != "" {
		_ = r.store.Set("vehicle.renault_gmid", gmid)
	}
	if ucid != "" {
		_ = r.store.Set("vehicle.renault_ucid", ucid)
	}
}

// gigyaPost issues a POST form to a Gigya endpoint, checks the structured
// errorCode, and decodes the body into out (when non-nil).
func (r *Renault) gigyaPost(op, endpoint string, form url.Values, out any) error {
	form.Set("apiKey", gigyaAPIKey)
	resp, err := r.client.PostForm(r.gigyaURL+endpoint, form)
	if err != nil {
		return fmt.Errorf("gigya %s: %w", op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("gigya %s read: %w", op, err)
	}

	var base struct {
		ErrorCode    int    `json:"errorCode"`
		ErrorMessage string `json:"errorMessage"`
		ErrorDetails string `json:"errorDetails"`
		RegToken     string `json:"regToken"`
	}
	if err := json.Unmarshal(body, &base); err != nil {
		return fmt.Errorf("gigya %s decode: %w", op, err)
	}
	if base.ErrorCode != 0 {
		return &gigyaError{op: op, code: base.ErrorCode, message: base.ErrorMessage, details: base.ErrorDetails, regToken: base.RegToken}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("gigya %s decode: %w", op, err)
		}
	}
	return nil
}

// bootstrapDevice fetches a fresh gmid/ucid device context from Gigya. These
// identify the "browser" to the TFA flow and, once trusted, let future logins
// skip the email challenge.
func (r *Renault) bootstrapDevice() (string, string, error) {
	resp, err := r.client.PostForm(r.gigyaURL+"/accounts.webSdkBootstrap", url.Values{"apiKey": {gigyaAPIKey}})
	if err != nil {
		return "", "", fmt.Errorf("gigya bootstrap: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var gmid, ucid string
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "gmid":
			gmid = c.Value
		case "ucid":
			ucid = c.Value
		}
	}

	// Some tenants return the ids in the body rather than as cookies.
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		GMID      string `json:"gmid"`
		UCID      string `json:"ucid"`
		ErrorCode int    `json:"errorCode"`
	}
	_ = json.Unmarshal(body, &out)
	if gmid == "" {
		gmid = out.GMID
	}
	if ucid == "" {
		ucid = out.UCID
	}
	if gmid == "" {
		return "", "", fmt.Errorf("gigya bootstrap: no gmid (code %d)", out.ErrorCode)
	}
	return gmid, ucid, nil
}

// initTFA starts the email provider for the pending registration and returns a
// gigyaAssertion used by the subsequent calls.
func (r *Renault) initTFA(regToken string) (string, error) {
	form := url.Values{
		"regToken": {regToken},
		"provider": {"gigyaEmail"},
		"mode":     {"verify"},
	}
	r.addDevice(form)
	var out struct {
		GigyaAssertion string `json:"gigyaAssertion"`
	}
	if err := r.gigyaPost("initTFA", "/accounts.tfa.initTFA", form, &out); err != nil {
		return "", err
	}
	if out.GigyaAssertion == "" {
		return "", fmt.Errorf("gigya initTFA: no assertion returned")
	}
	return out.GigyaAssertion, nil
}

// getTFAEmails lists the registered TFA email addresses and returns the first
// one's id plus its obfuscated form (for display).
func (r *Renault) getTFAEmails(assertion string) (string, string, error) {
	form := url.Values{"gigyaAssertion": {assertion}}
	var out struct {
		Emails []struct {
			ID         string `json:"id"`
			Obfuscated string `json:"obfuscated"`
		} `json:"emails"`
	}
	if err := r.gigyaPost("getEmails", "/accounts.tfa.email.getEmails", form, &out); err != nil {
		return "", "", err
	}
	if len(out.Emails) == 0 {
		return "", "", fmt.Errorf("gigya getEmails: no registered TFA email")
	}
	return out.Emails[0].ID, out.Emails[0].Obfuscated, nil
}

// sendTFACode emails a verification code to the given email id and returns a
// phvToken that ties the code back to this attempt.
func (r *Renault) sendTFACode(emailID, assertion string) (string, error) {
	form := url.Values{
		"emailID":        {emailID},
		"gigyaAssertion": {assertion},
		"lang":           {"en"},
	}
	var out struct {
		PhvToken string `json:"phvToken"`
	}
	if err := r.gigyaPost("sendCode", "/accounts.tfa.email.sendVerificationCode", form, &out); err != nil {
		return "", err
	}
	if out.PhvToken == "" {
		return "", fmt.Errorf("gigya sendVerificationCode: no phvToken returned")
	}
	return out.PhvToken, nil
}

// completeTFAVerification submits the emailed code and returns a
// providerAssertion proving the email was verified.
func (r *Renault) completeTFAVerification(assertion, phvToken, code string) (string, error) {
	form := url.Values{
		"gigyaAssertion": {assertion},
		"phvToken":       {phvToken},
		"code":           {code},
	}
	var out struct {
		ProviderAssertion string `json:"providerAssertion"`
	}
	if err := r.gigyaPost("completeVerification", "/accounts.tfa.email.completeVerification", form, &out); err != nil {
		return "", err
	}
	if out.ProviderAssertion == "" {
		return "", fmt.Errorf("gigya completeVerification: no providerAssertion returned")
	}
	return out.ProviderAssertion, nil
}

// finalizeTFA finalizes the verified TFA against the pending registration.
// tempDevice=false marks the device trusted (≈30 days); true keeps it one-shot.
func (r *Renault) finalizeTFA(assertion, providerAssertion, regToken string, tempDevice bool) error {
	form := url.Values{
		"gigyaAssertion":    {assertion},
		"providerAssertion": {providerAssertion},
		"regToken":          {regToken},
		"tempDevice":        {strconv.FormatBool(tempDevice)},
	}
	r.addDevice(form)
	return r.gigyaPost("finalizeTFA", "/accounts.tfa.finalizeTFA", form, nil)
}
