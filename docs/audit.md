# The record of who read what

Every request that puts a document in front of somebody leaves a record behind.
That includes the ones that did not: a document somebody was refused, a document that was not there, and a request that failed while it was being served.
A trail that only holds the successes answers the easy half of every question anybody asks it.

The rule is written down as a test rather than as a convention.
`api/surface_test.go` walks the server's route table, and a route that says it serves content and has no way to be exercised fails the walk.
A route that says nothing at all fails it too, so a new endpoint cannot avoid the question by not answering it.
The second half of the same test reads back what was written and fails if a document title, a document body or a group name appears anywhere in it.

## What is on a record

```json
{
  "at": "2026-08-19T09:14:22.104Z",
  "tenant": "acme",
  "subject": "u_mei",
  "kind": "user",
  "surface": "GET /api/v1/documents/{id}/content",
  "action": "content",
  "outcome": "served",
  "documents": [{"id": "d_4194", "source": "gdrive"}],
  "count": 1,
  "rule": "listed",
  "bytes": 184320
}
```

| Field | Meaning |
| --- | --- |
| `at` | when the server answered, in UTC |
| `tenant` | the tenant the request was made in |
| `subject` | the stable identifier of the caller, not their address |
| `kind` | `user`, `service` or `agent` |
| `surface` | the route pattern, so the record says how the document was reached |
| `action` | `search`, `suggest`, `list`, `read`, `content` or `thumbnail` |
| `outcome` | `served`, `refused` or `failed` |
| `query` | what was typed, on the search and suggest surfaces only |
| `documents` | the ids and sources of what was actually returned |
| `count` | how many matched, which is larger than `documents` on a paged search |
| `rule` | the rule that admitted the caller, on a served single document |
| `bytes` | how much content left the process |

What is deliberately not on a record is the content.
No title, no body, no snippet, no filename.
An audit trail is kept for years, is copied into ticketing systems and is read by more people than the corpus is, so a title in it is a leak with a long tail.
Group names are left off for the same reason one step further out: the list of groups that admitted somebody describes the shape of the organisation to everybody holding the export.
The `rule` is on the record because "they own it" and "it is public to the tenant" are different facts about an access and an investigation has to be able to tell them apart, and the reference that matched is not, because that reference is a group name.

The documents on a record are the ones that came back, never the ones that were asked for.
A search is filtered by the storage driver before the handler sees it, so a document the caller may not read cannot reach their record.
The exception is a refusal, where the id that was asked for is the only thing there is to say.

A refusal and a document that does not exist look identical on the trail, in the same way they look identical to the caller.
Separating them would prove a document exists to everybody who can read the trail, which is more people than can read the document.

## Where the records go

```
genbad -audit-dir /var/lib/genba/audit -audit-retention 2160h
```

| Setting | Variable | Default |
| --- | --- | --- |
| `-audit-dir` | `GENBA_AUDIT_DIR` | empty, which writes to the process log |
| `-audit-retention` | `GENBA_AUDIT_RETENTION` | zero, which keeps every day forever |

There is no setting that turns the trail off.
A deployment chooses where its records are kept and not whether any are written, so a server built with no options at all still writes to something, and `api.New` has no option that takes it away.
The default destination is the process log, under the message `content access`, which is the right answer on a laptop and behind a collector that is already shipping the log somewhere.

A directory is one file per UTC day, named `audit-2026-08-19.jsonl`, one JSON object per line.
The directory is created with mode 0700 and the files with mode 0600, and a directory that cannot be written is a startup failure rather than a fallback to the log.
Coming up anyway would be the same server serving the same content under a promise it is not keeping.

A retention with no directory is refused at startup for the same reason.
It reads like a statement about how long the trail is kept while the records are going to the log, whatever holds that log decides how long they live, and the setting deletes nothing.

## What a busy server does

Writes go through a queue so that a slow disk does not sit on the request path.
Two things about that queue are worth knowing.

A full queue blocks the request rather than dropping the record.
The alternative is a server that answers faster by keeping fewer records under exactly the load somebody would later be asked about.

A sink that returns an error drops the record and counts it rather than failing the request.
That is the one place the trail loses something, which is why the count is published and alerted on.

| Metric | What it is |
| --- | --- |
| `genba_audit_records_total` | records written |
| `genba_audit_failed_total` | records that could not be written, which should be flat at zero |
| `genba_audit_queued_records` | records waiting, which should be near zero |

`AuditRecordsAreBeingLost` in `docs/alerts.yml` fires on any of the second one.
It is one of the two alerts in the file because it is the only failure here that cannot be recovered by looking again later.

## Reading it back

```go
err := audit.Records(dir, from, to, func(rec audit.Record) error {
	fmt.Println(rec.Subject, rec.Action, rec.Documents)
	return nil
})
```

`Records` walks the days that overlap the window, oldest first, and calls the function for each record inside it.
The window is inclusive at both ends.
Returning an error from the function stops the walk and is returned, which is how a caller reads the first page of a large window without holding the rest of it.

`Export` is the same walk written to an `io.Writer`, in the format it is stored in, which means an export is concatenation and not a translation.
The reason it is not CSV is that the next question is always one the columns did not anticipate, and a JSON Lines file is something a person can grep and a spreadsheet can still import.

A half written last line is tolerated, because a process that was killed mid write leaves one and the records before it are still evidence.
A bad line anywhere else is an error naming the line number, because a corrupt file in the middle of a window is a different problem and quietly skipping it would produce an export that looks complete.

Files that are not part of the trail are left alone, both by the reader and by the retention sweep.
A day is deleted only once the whole day it holds is outside the window, so a retention of a week never deletes a record that is six days old.
