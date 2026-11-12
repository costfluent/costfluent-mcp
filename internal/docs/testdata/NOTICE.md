# Test data

`llms-full.txt` is a cut of the file the Costfluent docs portal publishes at
`https://docs.costfluent.com/llms-full.txt`, fetched 2026-11-10 (served `Last-Modified: Mon, 09 Nov
2026 07:30:41 GMT`): its header and four of its 140 pages, byte for byte and in their published
order, `/developers/terraform`, `/connect/azure`, `/de/developers/terraform` and `/de/connect/azure`.
It is Costfluent's own documentation, public on the portal, with no third-party text. Refresh it
from the live file the same way when a test needs a page it lacks; no page in the published file
holds a fenced `## ` line or a repeated heading, so the tests write those cases inline.
