package thread_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/genba/acl"
	"github.com/tamnd/genba/connector/thread"
	"github.com/tamnd/genba/doc"
)

// epoch is where every conversation in these tests starts, so that the times
// are arithmetic rather than a reading of the machine.
var epoch = time.Date(2026, time.May, 4, 10, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return epoch.Add(time.Duration(minutes) * time.Minute) }

func person(name string) doc.Person {
	return doc.Person{
		Name:     name,
		Identity: acl.Identity{Source: "chat", Value: strings.ToLower(name)},
	}
}

func perms() acl.Permissions {
	return acl.Permissions{
		Mode:        acl.ModeACL,
		Source:      "chat",
		AllowGroups: []acl.Ref{{Source: "chat", Value: "C-maintenance"}},
	}
}

// sample is the conversation most of these tests work on: somebody asks a
// question, two people answer, and the answer is at the bottom.
func sample() thread.Conversation {
	return thread.Conversation{
		ID:        "chat:C-maintenance:1714816800",
		Container: "maintenance",
		URL:       "https://chat.example.com/maintenance/p1714816800",
		Root: thread.Message{
			ID:     "m1",
			Author: person("Mei"),
			At:     at(0),
			Text:   "the gearbox on line two is making a noise again",
		},
		Replies: []thread.Message{
			{ID: "m2", Author: person("Ade"), At: at(5), Text: "did anybody check the oil level"},
			{ID: "m3", Author: person("Mei"), At: at(9), Text: "oil is fine, it is the bearing"},
		},
	}
}

// The whole point of the package. A conversation is one document, and every
// word anybody said in it is a word that can find it.
func TestAConversationIsOneDocumentThatHoldsEveryReply(t *testing.T) {
	got := sample().Document(perms())

	if got.ID != "chat:C-maintenance:1714816800" {
		t.Errorf("id is %q", got.ID)
	}
	for _, want := range []string{"gearbox", "oil level", "it is the bearing"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("the body does not contain %q:\n%s", want, got.Body)
		}
	}
	if got.Kind != doc.KindMessage {
		t.Errorf("kind is %q, want a message", got.Kind)
	}
	if got.Container != "maintenance" {
		t.Errorf("container is %q", got.Container)
	}
	if got.URL == "" {
		t.Error("the document does not link back to the conversation")
	}
}

// The author of a message is in the body in front of what they said, which is
// the difference between searching for what Mei said about the gearbox and only
// being able to search for the gearbox.
func TestEachMessageIsAttributedInTheBody(t *testing.T) {
	got := sample().Document(perms())
	want := "Mei: the gearbox on line two is making a noise again\n\n" +
		"Ade: did anybody check the oil level\n\n" +
		"Mei: oil is fine, it is the bearing"
	if got.Body != want {
		t.Errorf("body is\n%s\nwant\n%s", got.Body, want)
	}
}

func TestAMessageWithNoNameToPutOnItIsStillIndexed(t *testing.T) {
	c := sample()
	c.Replies = append(c.Replies, thread.Message{ID: "m4", At: at(12), Text: "replacement ordered"})

	got := c.Document(perms())
	if !strings.HasSuffix(got.Body, "\n\nreplacement ordered") {
		t.Errorf("the message was dropped or was given an empty name:\n%s", got.Body)
	}
}

func TestTheNameFallsBackToWhateverTheSourceKnew(t *testing.T) {
	tests := []struct {
		name   string
		author doc.Person
		want   string
	}{
		{"a display name", doc.Person{Name: "Mei Chen", Email: "mei@example.com"}, "Mei Chen: hello"},
		{"an email address", doc.Person{Email: "mei@example.com"}, "mei@example.com: hello"},
		{"a source identifier", doc.Person{Identity: acl.Identity{Source: "chat", Value: "U04AB"}}, "U04AB: hello"},
		{"nothing at all", doc.Person{}, "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := thread.Conversation{
				ID:   "chat:x",
				Root: thread.Message{ID: "m1", Author: tt.author, At: at(0), Text: "hello"},
			}
			if got := c.Document(perms()).Body; got != tt.want {
				t.Errorf("body is %q, want %q", got, tt.want)
			}
		})
	}
}

// A paged reply listing at more than one source repeats the parent message on
// every page, so a connector that concatenates the pages hands the root over
// three times. Saying it three times in the body would also treble its weight
// in the ranking.
func TestARepeatedMessageIsKeptOnce(t *testing.T) {
	c := sample()
	c.Replies = append([]thread.Message{c.Root, c.Replies[0], c.Root}, c.Replies...)

	got := c.Document(perms())
	if n := strings.Count(got.Body, "the gearbox on line two"); n != 1 {
		t.Errorf("the root appears %d times:\n%s", n, got.Body)
	}
	if n := strings.Count(got.Body, "did anybody check the oil"); n != 1 {
		t.Errorf("a reply appears %d times:\n%s", n, got.Body)
	}
	if got.Properties["messages"] != "3" {
		t.Errorf("the document counts %s messages, want 3", got.Properties["messages"])
	}
}

func TestRepliesAreOrderedByWhenTheyWereWritten(t *testing.T) {
	c := sample()
	c.Replies = []thread.Message{c.Replies[1], c.Replies[0]}

	body := c.Document(perms()).Body
	if i, j := strings.Index(body, "oil level"), strings.Index(body, "it is the bearing"); i > j {
		t.Errorf("the replies came out in the wrong order:\n%s", body)
	}
}

// A ticket description edited after the first comment carries a later time than
// the comment, and an imported conversation carries whatever time the import
// wrote. The root is still what the conversation is about.
func TestTheRootStaysFirstWhateverItsTimestampSays(t *testing.T) {
	c := sample()
	c.Root.At = at(30)

	body := c.Document(perms()).Body
	if !strings.HasPrefix(body, "Mei: the gearbox") {
		t.Errorf("the root is not first:\n%s", body)
	}
}

func TestTwoMessagesWrittenInTheSameInstantComeOutInTheSameOrderTwice(t *testing.T) {
	c := sample()
	c.Replies = []thread.Message{
		{ID: "m9", Author: person("Zo"), At: at(5), Text: "last"},
		{ID: "m2", Author: person("Ade"), At: at(5), Text: "first"},
	}
	first := c.Document(perms()).Body
	for range 5 {
		if got := c.Document(perms()).Body; got != first {
			t.Fatalf("the body changed between runs:\n%s\n%s", first, got)
		}
	}
	if strings.Index(first, "first") > strings.Index(first, "last") {
		t.Errorf("the tie was broken by something other than the message id:\n%s", first)
	}
}

func TestTheConversationSpansFromTheFirstMessageToTheLast(t *testing.T) {
	got := sample().Document(perms())
	if !got.CreatedAt.Equal(at(0)) {
		t.Errorf("created at %v, want %v", got.CreatedAt, at(0))
	}
	if !got.ModifiedAt.Equal(at(9)) {
		t.Errorf("modified at %v, want %v", got.ModifiedAt, at(9))
	}
}

// A message edited in place changes what the document says without any reply
// being added. A version derived only from reply times would leave the index
// serving the old text until somebody happened to answer.
func TestAnEditMovesTheVersionWithoutAnyReply(t *testing.T) {
	before := sample().Document(perms())

	c := sample()
	c.Replies[0].Edited = at(40)
	after := c.Document(perms())

	if after.SourceUpdate == before.SourceUpdate {
		t.Errorf("the version is still %q after a message was edited", after.SourceUpdate)
	}
	if !after.ModifiedAt.Equal(at(40)) {
		t.Errorf("modified at %v, want the time of the edit", after.ModifiedAt)
	}
	if !after.CreatedAt.Equal(at(0)) {
		t.Errorf("the edit moved the creation time to %v", after.CreatedAt)
	}
}

func TestASourceThatKeepsItsOwnVersionKeepsIt(t *testing.T) {
	c := sample()
	c.Revision = "17"
	if got := c.Document(perms()).SourceUpdate; got != "17" {
		t.Errorf("version is %q, want the source's own", got)
	}
}

func TestAConversationWithNoTimesHasNoVersionToOffer(t *testing.T) {
	c := thread.Conversation{ID: "chat:x", Root: thread.Message{ID: "m1", Text: "hello"}}
	if got := c.Document(perms()).SourceUpdate; got != "" {
		t.Errorf("version is %q, want nothing rather than the zero time", got)
	}
}

func TestTheTitleComesFromTheFirstMessageWhenTheSourceHasNone(t *testing.T) {
	if got := sample().Document(perms()).Title; got != "the gearbox on line two is making a noise again" {
		t.Errorf("title is %q", got)
	}
}

func TestASourceWithItsOwnTitleKeepsIt(t *testing.T) {
	c := sample()
	c.Title = "Line two gearbox noise"
	if got := c.Document(perms()).Title; got != "Line two gearbox noise" {
		t.Errorf("title is %q", got)
	}
}

func TestALongFirstMessageIsElidedIntoATitle(t *testing.T) {
	c := sample()
	c.Root.Text = strings.Repeat("the gearbox is making a noise and ", 20)

	got := c.Document(perms()).Title
	switch {
	case len([]rune(got)) > thread.TitleLimit+3:
		t.Errorf("title is %d runes: %q", len([]rune(got)), got)
	case !strings.HasSuffix(got, "..."):
		t.Errorf("title %q does not say it was cut", got)
	case strings.HasSuffix(strings.TrimSuffix(got, "..."), " "):
		t.Errorf("title %q was cut in the middle of the space", got)
	}
}

func TestOnlyTheFirstLineOfAMessageBecomesATitle(t *testing.T) {
	c := sample()
	c.Root.Text = "gearbox noise\n\nit started on Tuesday and it is worse under load"

	if got := c.Document(perms()).Title; got != "gearbox noise" {
		t.Errorf("title is %q", got)
	}
}

// A conversation that opened with a file or an image has nothing readable in
// its first message. The container is a worse title than the thread deserves
// and a great deal better than an empty result row.
func TestAConversationWithNothingToNameItselfAfterUsesItsContainer(t *testing.T) {
	c := thread.Conversation{
		ID:        "chat:x",
		Container: "maintenance",
		Root:      thread.Message{ID: "m1", At: at(0)},
		Replies:   []thread.Message{{ID: "m2", At: at(1), Text: "thanks"}},
	}
	if got := c.Document(perms()).Title; got != "maintenance" {
		t.Errorf("title is %q", got)
	}
}

func TestTheCountsAreOnTheDocument(t *testing.T) {
	got := sample().Document(perms())
	want := map[string]string{
		"messages":     "3",
		"replies":      "2",
		"participants": "2",
	}
	for k, v := range want {
		if got.Properties[k] != v {
			t.Errorf("%s is %q, want %q", k, got.Properties[k], v)
		}
	}
	if got.Properties[doc.MediaType] != "text/plain" {
		t.Errorf("media type is %q", got.Properties[doc.MediaType])
	}
}

func TestTheConnectorsOwnPropertiesSurvive(t *testing.T) {
	c := sample()
	c.Properties = map[string]string{"status": "open", doc.MediaType: "text/markdown"}

	got := c.Document(perms()).Properties
	if got["status"] != "open" {
		t.Errorf("the connector's own property is %q", got["status"])
	}
	if got[doc.MediaType] != "text/markdown" {
		t.Errorf("media type is %q, want the one the connector set", got[doc.MediaType])
	}
	if got["messages"] != "3" {
		t.Errorf("the counts were not added alongside")
	}
}

func TestTheCallersPropertyMapIsNotWrittenTo(t *testing.T) {
	c := sample()
	c.Properties = map[string]string{"status": "open"}

	c.Document(perms())
	if len(c.Properties) != 1 {
		t.Errorf("the caller's map grew to %v", c.Properties)
	}
}

// The middle is what gets dropped. The root stays because it is what the
// conversation is about, and the end stays because a thread long enough to be
// cut is usually one that took a while to work something out, and the working
// out is at the bottom.
func TestALongConversationKeepsItsBeginningAndItsEnd(t *testing.T) {
	c := thread.Conversation{
		ID:   "chat:long",
		Root: thread.Message{ID: "m0", Author: person("Mei"), At: at(0), Text: "why was the gearbox order cancelled"},
	}
	for i := 1; i <= 200; i++ {
		c.Replies = append(c.Replies, thread.Message{
			ID:     "m" + strconv.Itoa(i),
			Author: person("Ade"),
			At:     at(i),
			Text:   "message number " + strconv.Itoa(i) + " " + strings.Repeat("filler ", 20),
		})
	}
	c.Replies = append(c.Replies, thread.Message{
		ID:     "m201",
		Author: person("Mei"),
		At:     at(201),
		Text:   "the supplier could not meet the date",
	})

	got := c.Document(perms(), thread.WithMaxBody(4096))
	if len(got.Body) > 4096 {
		t.Fatalf("the body is %d bytes, over the limit", len(got.Body))
	}
	if !strings.Contains(got.Body, "why was the gearbox order cancelled") {
		t.Error("the root was dropped")
	}
	if !strings.Contains(got.Body, "the supplier could not meet the date") {
		t.Error("the answer at the bottom was dropped")
	}
	if strings.Contains(got.Body, "message number 100 ") {
		t.Error("nothing was dropped, so the limit did nothing")
	}
	if got.Properties["truncated"] != "true" {
		t.Error("the document does not say it holds part of the conversation")
	}
	if got.Properties["omitted_messages"] == "" || got.Properties["omitted_messages"] == "0" {
		t.Errorf("omitted_messages is %q", got.Properties["omitted_messages"])
	}
}

// Nothing is written in place of what was left out. A marker in a body is a
// phrase in the index that nobody at the source ever typed, and it turns up in
// snippets.
func TestNothingIsWrittenInPlaceOfWhatWasDropped(t *testing.T) {
	c := thread.Conversation{
		ID:   "chat:long",
		Root: thread.Message{ID: "m0", Author: person("Mei"), At: at(0), Text: "start"},
	}
	for i := 1; i <= 50; i++ {
		c.Replies = append(c.Replies, thread.Message{
			ID: "m" + strconv.Itoa(i), Author: person("Ade"), At: at(i),
			Text: strings.Repeat("filler ", 20),
		})
	}
	body := c.Document(perms(), thread.WithMaxBody(512)).Body
	for _, marker := range []string{"[", "…", "...", "omitted", "truncated", "snip"} {
		if strings.Contains(body, marker) {
			t.Errorf("the body carries %q, which nobody typed:\n%s", marker, body)
		}
	}
}

// A conversation with a very long first message and nothing else still has to
// produce a body, and it still has to respect the limit.
func TestAFirstMessageOverTheLimitIsCutRatherThanDropped(t *testing.T) {
	c := thread.Conversation{
		ID:   "chat:x",
		Root: thread.Message{ID: "m1", Author: person("Mei"), At: at(0), Text: strings.Repeat("a", 4000)},
	}
	got := c.Document(perms(), thread.WithMaxBody(100))
	if got.Body == "" {
		t.Fatal("the body is empty")
	}
	if len(got.Body) > 100 {
		t.Errorf("the body is %d bytes, over the limit", len(got.Body))
	}
}

func TestCuttingABodyDoesNotSplitARune(t *testing.T) {
	c := thread.Conversation{
		ID:   "chat:x",
		Root: thread.Message{ID: "m1", At: at(0), Text: strings.Repeat("\u00e9", 200)},
	}
	body := c.Document(perms(), thread.WithMaxBody(101)).Body
	if strings.ContainsRune(body, '\ufffd') {
		t.Errorf("the body was cut in the middle of a rune: %q", body)
	}
}

func TestAConversationThatFitsIsNotMarkedAsCut(t *testing.T) {
	got := sample().Document(perms())
	if _, ok := got.Properties["truncated"]; ok {
		t.Error("a conversation that fits was marked as cut")
	}
	if _, ok := got.Properties["omitted_messages"]; ok {
		t.Error("a conversation that fits reported omitted messages")
	}
}

func TestAMessageWithNoTextIsNotARunOfBlankLines(t *testing.T) {
	c := sample()
	c.Replies = append(c.Replies, thread.Message{ID: "m4", Author: person("Zo"), At: at(11), Text: "   "})

	got := c.Document(perms())
	if strings.Contains(got.Body, "\n\n\n") {
		t.Errorf("an empty message left a hole in the body:\n%q", got.Body)
	}
	if strings.Contains(got.Body, "Zo:") {
		t.Errorf("an empty message was attributed:\n%q", got.Body)
	}
	// It is still a message somebody sent, and it still counts.
	if got.Properties["messages"] != "4" {
		t.Errorf("the document counts %s messages, want 4", got.Properties["messages"])
	}
}

// The permissions are an argument rather than a field, so that a connector
// cannot assemble a thread and forget to say who may read it.
func TestThePermissionsAreWhateverTheConnectorPassed(t *testing.T) {
	got := sample().Document(perms())
	if got.Permissions.Mode != acl.ModeACL || got.Permissions.Source != "chat" {
		t.Errorf("permissions are %+v", got.Permissions)
	}
	if len(got.Permissions.AllowGroups) != 1 {
		t.Errorf("the allow list is %v", got.Permissions.AllowGroups)
	}
}

// The tenant is the pipeline's to set and the source name is the connector's.
// Neither belongs to a conversation, and filling either in here would put a
// value somewhere the layer above overwrites.
func TestTheTenantAndTheSourceAreLeftToTheLayersThatOwnThem(t *testing.T) {
	got := sample().Document(perms())
	if got.Tenant != "" {
		t.Errorf("tenant is %q", got.Tenant)
	}
	if got.Source != "" {
		t.Errorf("source is %q", got.Source)
	}
}

func TestTheKindTheConnectorAskedForIsKept(t *testing.T) {
	c := sample()
	c.Kind = doc.KindTicket
	if got := c.Document(perms()).Kind; got != doc.KindTicket {
		t.Errorf("kind is %q", got)
	}
}

func TestTheAuthorIsWhoeverStartedIt(t *testing.T) {
	got := sample().Document(perms())
	if got.Author.Name != "Mei" {
		t.Errorf("author is %+v", got.Author)
	}
}

// Two people with the same display name are two people if the source gave them
// different identifiers, and one person if it did not.
func TestParticipantsAreCountedByIdentityRatherThanByName(t *testing.T) {
	c := thread.Conversation{
		ID:   "chat:x",
		Root: thread.Message{ID: "m1", At: at(0), Text: "one", Author: doc.Person{Name: "Alex", Identity: acl.Identity{Source: "chat", Value: "U1"}}},
		Replies: []thread.Message{
			{ID: "m2", At: at(1), Text: "two", Author: doc.Person{Name: "Alex", Identity: acl.Identity{Source: "chat", Value: "U2"}}},
			{ID: "m3", At: at(2), Text: "three", Author: doc.Person{Name: "Alex", Identity: acl.Identity{Source: "chat", Value: "U1"}}},
		},
	}
	if got := c.Document(perms()).Properties["participants"]; got != "2" {
		t.Errorf("participants is %q, want 2", got)
	}
}

func TestAnEmptyConversationProducesAnEmptyBodyRatherThanPanicking(t *testing.T) {
	got := thread.Conversation{ID: "chat:x"}.Document(perms())
	if got.Body != "" {
		t.Errorf("body is %q", got.Body)
	}
	if got.Properties["messages"] != "0" {
		t.Errorf("messages is %q", got.Properties["messages"])
	}
}

func TestAMaxBodyBelowOneSelectsTheDefault(t *testing.T) {
	got := sample().Document(perms(), thread.WithMaxBody(0), thread.WithMaxBody(-1))
	if !strings.Contains(got.Body, "it is the bearing") {
		t.Errorf("a limit of zero cut the body:\n%s", got.Body)
	}
}
