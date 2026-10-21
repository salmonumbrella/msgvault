# Synthetic PST fixtures

These fixtures are built from scratch by `generate_pst.py`. They contain
synthetic emails and reserved example addresses. No bytes come from an existing
archive or correspondence.

The small, unencrypted Unicode fixtures exercise msgvault's native PST reader,
MIME conversion and importer. The media fixture includes one attachment. The
broken-media fixture deliberately omits the attachment's subnodes to exercise
native parser failure and retry handling. The reply-chain fixture contains a
root, an attached intermediate reply and a final reply, all in one folder.

Regenerate them in this directory:

```sh
python3 generate_pst.py synthetic-message.pst plain
python3 generate_pst.py synthetic-media.pst media
python3 generate_pst.py synthetic-broken-media.pst broken
python3 generate_pst.py synthetic-reply-chain.pst reply-chain
```
