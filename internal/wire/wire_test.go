package wire

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/n0rm4l-me/petrvs-bridge/internal/ratchet"
)

func pair(t testing.TB) (agent, hub *ratchet.Ratchet) {
	t.Helper()
	var sk [32]byte
	rand.Read(sk[:])
	hubKey, err := ecdhKey()
	if err != nil {
		t.Fatal(err)
	}
	agent, err = ratchet.NewInitiator(sk, hubKey.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return agent, ratchet.NewResponder(sk, hubKey)
}

var sid = [8]byte{1, 2, 3, 4, 5, 6, 7, 8}

func p(t Type, body string) *Payload {
	now := time.Now().Unix()
	return &Payload{ID: "id-1", Type: t, Body: body, TS: now, Exp: now + 60}
}

func TestSealOpenRoundTrip(t *testing.T) {
	agent, hub := pair(t)
	data, err := Seal(agent, "laptop", "hub", sid, p(TypeReady, "hello"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(hub, "laptop", "hub", sid, data, true)
	if err != nil || got.Type != TypeReady || got.Body != "hello" {
		t.Fatalf("%v %+v", err, got)
	}
	data, _ = Seal(hub, "hub", "laptop", sid, p(TypeText, "status"))
	if got, err := Open(agent, "hub", "laptop", sid, data, false); err != nil || got.Body != "status" {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestDirectionIsEnforced(t *testing.T) {
	agent, hub := pair(t)
	// an agent-side type arriving where commands are expected must be refused
	data, _ := Seal(agent, "laptop", "hub", sid, p(TypeReply, "x"))
	if _, err := Open(hub, "laptop", "hub", sid, data, false); !errors.Is(err, ErrPayload) {
		t.Fatalf("a reply was accepted in the command direction: %v", err)
	}
	agent2, hub2 := pair(t)
	data, _ = Seal(hub2, "hub", "laptop", sid, p(TypeText, "x")) // hub2 cannot send yet
	_ = data
	m, _ := Seal(agent2, "laptop", "hub", sid, p(TypeReady, ""))
	Open(hub2, "laptop", "hub", sid, m, true)
	cmd, _ := Seal(hub2, "hub", "laptop", sid, p(TypeText, "run"))
	if _, err := Open(agent2, "hub", "laptop", sid, cmd, true); !errors.Is(err, ErrPayload) {
		t.Fatalf("a command was accepted in the report direction: %v", err)
	}
	_ = agent
}

func TestMessageCannotBeMovedToAnotherDirectionOrSession(t *testing.T) {
	agent, hub := pair(t)
	data, _ := Seal(agent, "laptop", "hub", sid, p(TypeReady, ""))
	if _, err := Open(hub, "hub", "laptop", sid, data, true); err == nil {
		t.Fatal("a message was accepted with sender and recipient swapped")
	}
	if _, err := Open(hub, "laptop", "hub", [8]byte{9}, data, true); err == nil {
		t.Fatal("a message was accepted for a different session id")
	}
	if _, err := Open(hub, "other", "hub", sid, data, true); err == nil {
		t.Fatal("a message was accepted from a different sender name")
	}
	if _, err := Open(hub, "laptop", "hub", sid, data, true); err != nil {
		t.Fatalf("and the real one still opens: %v", err)
	}
}

func TestSessionIDPeek(t *testing.T) {
	agent, _ := pair(t)
	data, _ := Seal(agent, "laptop", "hub", sid, p(TypeReady, ""))
	if got, err := SessionID(data); err != nil || got != sid {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][]byte{nil, {1}, {2, 0, 0, 0, 0, 0, 0, 0, 0}, make([]byte, MaxData+1)} {
		if _, err := SessionID(bad); err == nil {
			t.Errorf("%d bytes accepted", len(bad))
		}
	}
}

func TestPayloadValidation(t *testing.T) {
	now := time.Now().Unix()
	ok := Payload{ID: "x", Type: TypeText, TS: now, Exp: now + 1}
	if err := ok.Validate(false); err != nil {
		t.Fatal(err)
	}
	cases := map[string]Payload{
		"no id":         {Type: TypeText, TS: now, Exp: now + 1},
		"long id":       {ID: strings.Repeat("x", 65), Type: TypeText, TS: now, Exp: now + 1},
		"unknown type":  {ID: "x", Type: "rm", TS: now, Exp: now + 1},
		"big body":      {ID: "x", Type: TypeText, Body: strings.Repeat("x", MaxBody+1), TS: now, Exp: now + 1},
		"exp before ts": {ID: "x", Type: TypeText, TS: now, Exp: now - 1},
		"many meta":     {ID: "x", Type: TypeText, TS: now, Exp: now + 1, Meta: map[string]string{"1": "", "2": "", "3": "", "4": "", "5": "", "6": "", "7": "", "8": "", "9": "", "10": "", "11": "", "12": "", "13": "", "14": "", "15": "", "16": "", "17": ""}},
		"big meta":      {ID: "x", Type: TypeText, TS: now, Exp: now + 1, Meta: map[string]string{"k": strings.Repeat("v", 257)}},
	}
	for name, c := range cases {
		if err := c.Validate(false); !errors.Is(err, ErrPayload) {
			t.Errorf("%s: got %v, want ErrPayload", name, err)
		}
	}
}

func TestExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	pl := &Payload{TS: now.Unix(), Exp: now.Add(time.Minute).Unix()}
	if pl.Expired(now, 0) || pl.Expired(now.Add(time.Minute), 0) {
		t.Fatal("a payload expired early")
	}
	if !pl.Expired(now.Add(time.Minute+time.Second), 0) {
		t.Fatal("an expired payload was not detected")
	}
	if pl.Expired(now.Add(time.Minute+20*time.Second), 30*time.Second) {
		t.Fatal("clock skew allowance was not applied")
	}
}

func TestOpenRejectsMalformedPlaintextInsideValidCiphertext(t *testing.T) {
	agent, hub := pair(t)
	// a sender bug or a compromised peer: valid encryption, invalid content
	for name, raw := range map[string]string{"not json": "hello", "unknown field": `{"id":"x","type":"ready","ts":1,"exp":2,"extra":1}`, "trailing": `{"id":"x","type":"ready","ts":1,"exp":2} {}`} {
		blob, _ := agent.Encrypt([]byte(raw), AD("laptop", "hub", sid))
		data := append(append([]byte{Version}, sid[:]...), blob...)
		if _, err := Open(hub, "laptop", "hub", sid, data, true); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestAttrs(t *testing.T) {
	a := Attrs(KindMsg, "laptop")
	if a[AttrKind] != KindMsg || a[AttrAgent] != "laptop" || a[AttrVersion] != "1" {
		t.Fatalf("%v", a)
	}
}

func TestADoneReportTravelsOnlyFromTheAgentToTheHub(t *testing.T) {
	agent, hub := pair(t)
	data, err := Seal(agent, "laptop", "hub", sid, p(TypeDone, "cmd-1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(hub, "laptop", "hub", sid, data, true)
	if err != nil || got.Type != TypeDone || got.Body != "cmd-1" {
		t.Fatalf("%v %+v", err, got)
	}
	agent2, hub2 := pair(t)
	m, _ := Seal(agent2, "laptop", "hub", sid, p(TypeReady, ""))
	Open(hub2, "laptop", "hub", sid, m, true)
	back, err := Seal(hub2, "hub", "laptop", sid, p(TypeDone, "x"))
	if err == nil {
		if _, err = Open(agent2, "hub", "laptop", sid, back, false); !errors.Is(err, ErrPayload) {
			t.Fatalf("a done report was accepted in the command direction: %v", err)
		}
	}
}

func TestFileScreenAndStartTypesGoTheirOwnWay(t *testing.T) {
	ok := func(typ Type, toHub bool) bool {
		p := &Payload{ID: "x", Type: typ, TS: 1, Exp: 2}
		return p.Validate(toHub) == nil
	}
	for _, typ := range []Type{TypePart, TypeFile} {
		if !ok(typ, true) || !ok(typ, false) {
			t.Errorf("%s has to travel both ways", typ)
		}
	}
	for _, typ := range []Type{TypeScreen, TypeStart} {
		if !ok(typ, false) || ok(typ, true) {
			t.Errorf("%s goes to the agent only", typ)
		}
	}
}

func TestOnlyWhatChangesTheSessionIsNumbered(t *testing.T) {
	for typ, want := range map[Type]bool{TypeText: true, TypeKey: true, TypeFile: true, TypeStart: true, TypePart: false, TypeScreen: false, TypeLockdown: false, TypeAlarm: false, TypePing: false, TypeReply: false} {
		if IsCommand(typ) != want {
			t.Errorf("IsCommand(%s) = %v, want %v", typ, !want, want)
		}
	}
}

func TestButtonsAreBounded(t *testing.T) {
	base := func(bs ...Button) *Payload {
		return &Payload{ID: "x", Type: TypeNotice, TS: 1, Exp: 2, Buttons: bs}
	}
	if err := base(Button{Label: "1. Yes", Key: "1", Prompt: "0a1b2c3d"}).Validate(true); err != nil {
		t.Fatalf("a normal button: %v", err)
	}
	many := make([]Button, MaxButtons+1)
	for i := range many {
		many[i] = Button{Label: "b", Key: "1"}
	}
	for name, p := range map[string]*Payload{
		"too many":          base(many...),
		"no label":          base(Button{Key: "1"}),
		"a label too long":  base(Button{Label: strings.Repeat("x", 41), Key: "1"}),
		"a key too long":    base(Button{Label: "x", Key: strings.Repeat("k", 17)}),
		"a prompt too long": base(Button{Label: "x", Key: "1", Prompt: strings.Repeat("p", 17)}),
	} {
		if err := p.Validate(true); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}
