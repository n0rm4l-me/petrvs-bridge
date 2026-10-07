package screen

import (
	"regexp"
	"strings"
)

// What Redact knows about. It is a list of the forms in which secrets commonly show up in a command line or a screen, not
// a detector: a secret in a form that is not here goes to the chat. Each rule hides only the value, so that what is being
// run can still be read.

const (
	// stop ends an unquoted value: white space and what a shell reads as syntax. Without it a value swallows the
	// rest of the command ("export TOKEN=x;rm -rf ~" would show as "export TOKEN=<hidden>"), and the user approves what
	// they cannot see.
	stop = "\\s;|&<>()$`"
	// tail is what may sit between a keyword and the sign that follows it: the rest of a name (token_file, API_KEY_ID, a
	// closing quote).
	tail = "[^=:" + stop + "]*"
	// bare and quoted are a value as written in a shell or a config file.
	bare   = "[^" + stop + "]+"
	quoted = `"[^"\n]*"|'[^'\n]*'`
	value  = "(" + quoted + "|" + bare + ")"
	// sign is the separator in front of a value: a space is one for the words that are seldom used otherwise, and a value
	// may be on the next line.
	sign     = `(?:[=: \t]+|[=:][ \t]*\n[ \t]*)`
	signOnly = `(?:[=:][ \t]*(?:\n[ \t]*)?)`
)

var (
	pemRe = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]+-----.*?(?:-----END [A-Z0-9 ]+-----|\z)`)

	// shapeRe matches secrets by how they look, wherever they are. \b keeps "task-force" from being taken for "sk-".
	shapeRe = regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{6,}|github_pat_[A-Za-z0-9_]{6,}|glpat-[A-Za-z0-9_-]{8,}` +
		`|sk-[A-Za-z0-9_-]{12,}|[sr]k_(?:live|test)_[A-Za-z0-9]{6,}|xox[baprs]-[A-Za-z0-9-]{6,}|(?:AKIA|ASIA)[0-9A-Z]{16}` +
		`|AIza[0-9A-Za-z_-]{30,}|eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*)`)

	// userinfoRe is the "user:password@" (or "token@") of a URL. It runs to the last @ before the path, so a password that
	// has an @ in it is hidden whole.
	userinfoRe = regexp.MustCompile(`(://)([^/\s?#'"]*)@`)
	plainUser  = regexp.MustCompile(`^[a-z]{1,12}$`)

	// authRe hides what follows "authorization": a scheme and a credential, so two words.
	authRe = regexp.MustCompile(`(?i)(authorization` + tail + `[=: \t]+)("[^"\n]*"|'[^'\n]*'|` + bare + `(?:[ \t]+[^<` + stop + `]+)?)`)
	// cookieRe hides the whole cookie: name=value pairs with ";" between them, up to the end of the quote or the line.
	cookieRe = regexp.MustCompile(`(?i)(cookie` + tail + `[=:][ \t]*)([^'"\n]+)`)
	bearerRe = regexp.MustCompile(`(?i)((?:bearer|basic) +)` + bare)
	// secretRe is for words that are used for a secret and hardly for anything else, so a space after them is enough.
	secretRe = regexp.MustCompile(`(?i)((?:token|secret|passw(?:or)?d|passphrase|api[_-]?key)` + tail + sign + `)` + value)
	// assignRe is for short words that are a secret only in an assignment: "pass" alone is a word of a sentence, and
	// "PASS: TestX" is the output of a test run. They are taken only as a whole word of a name (DB_PASS, pat-token), not
	// inside another word, and only with "=".
	assignRe = regexp.MustCompile(`(?i)((?:^|[^a-z])(?:pass|pwd|pat)(?:[_-][a-z0-9_-]*)?["']?=[ \t]*)` + value)
	// namedRe is for longer words that are rarely used for anything else, also in "name: value" of a config file.
	namedRe = regexp.MustCompile(`(?i)((?:credentials?|private[_-]?key|session(?:[_-]?id)?)(?:[_-][a-z0-9_-]*)?["']?` + signOnly + `)` + value)
	// keyEnvRe is an environment variable named like SOMETHING_KEY, which by convention holds a key.
	keyEnvRe = regexp.MustCompile(`(\b[A-Z][A-Z0-9]*_KEY=)` + value)

	// Flags of programs that take a password or a token on the command line. They are tied to the program, because the
	// same letters mean other things elsewhere (mkdir -p, ssh -p 22, docker run -u 1000:1000).
	flagRes = []*regexp.Regexp{
		// curl -u user:password
		regexp.MustCompile(`(?i)(\bcurl\b[^\n;|&]*?\s(?:-u|--user|--proxy-user)[ =]*['"]?[^\s:'"]*:)([^\s'";|&<>()$` + "`" + `]+)`),
		// curl --cookie 'a=b'
		regexp.MustCompile(`(?i)(\bcurl\b[^\n;|&]*?\s(?:-b|--cookie)[ =]*)` + value),
		// mysql -pSECRET: attached, since "-p DATABASE" is not a password
		regexp.MustCompile(`(?i)(\b(?:mysql|mysqldump|mysqladmin|mysqlimport|mysqlcheck|mariadb)\b[^\n;|&]*?\s-p)([^\s;|&<>()$` + "`" + `]+)`),
		// sshpass -p x
		regexp.MustCompile(`(?i)(\bsshpass\b[^\n;|&]*?\s-p\s*)` + value),
		// docker login -p x, and the other logins
		regexp.MustCompile(`(?i)(\blogin\b[^\n;|&]*?\s-p\s*)` + value),
		// openssl enc -k x, -pass pass:x
		regexp.MustCompile(`(?i)(\bopenssl\b[^\n;|&]*?\s-k\s*)` + value),
		regexp.MustCompile(`(?i)(\bopenssl\b[^\n;|&]*?\s-pass(?:in|out)?\s+pass:)` + value),
	}
)

const hidden = "<hidden>"

// Redact hides what looks like a secret: the value after words like token, secret, password, passphrase, api key,
// authorization, cookie and bearer, the credentials of a URL, the password flag of some programs, PEM blocks, and tokens
// of well-known shapes. Best effort, not a guarantee, and it errs toward hiding too much. The command that follows a
// hidden value stays readable.
func Redact(s string) string {
	s = pemRe.ReplaceAllString(s, hidden)
	s = shapeRe.ReplaceAllString(s, hidden)
	s = userinfoRe.ReplaceAllStringFunc(s, hideUserinfo)
	s = authRe.ReplaceAllString(s, "${1}"+hidden)
	s = cookieRe.ReplaceAllString(s, "${1}"+hidden)
	s = bearerRe.ReplaceAllString(s, "${1}"+hidden)
	s = secretRe.ReplaceAllString(s, "${1}"+hidden)
	s = assignRe.ReplaceAllString(s, "${1}"+hidden)
	s = namedRe.ReplaceAllString(s, "${1}"+hidden)
	s = keyEnvRe.ReplaceAllString(s, "${1}"+hidden)
	for _, re := range flagRes {
		s = re.ReplaceAllString(s, "${1}"+hidden)
	}
	return s
}

// hideUserinfo hides the password of "user:password@", or all of "token@": a name before the @ with no password is kept
// only when it is a plain short user name (git@, deploy@).
func hideUserinfo(m string) string {
	sub := userinfoRe.FindStringSubmatch(m)
	user, _, hasPassword := strings.Cut(sub[2], ":")
	switch {
	case hasPassword:
		return sub[1] + user + ":" + hidden + "@"
	case plainUser.MatchString(sub[2]):
		return m
	}
	return sub[1] + hidden + "@"
}
