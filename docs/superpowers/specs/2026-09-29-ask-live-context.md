# Ask live and recent context

Ask uses the selected child's local store at submission time. Each record-backed turn receives a fresh, explicit snapshot of every running feed and sleep timer, including the current feeding step, its start, and the snapshot time. A running sleep timer is evidence that sleep is logged as in progress, not direct observation of the child.

The default record context contains a chronological 48-hour care timeline with source-linked feed steps, measured bottle volume, sleep, diapers, and supplements. Existing weekly and daily totals remain available for trend questions. If the rendered timeline exceeds the model's usable context, Ask states the omitted interval in its internal context and retrieves that interval before answering a question that depends on it. It never silently treats a truncated prompt as complete history.

The read-only local log tool can retrieve bounded explicit date ranges, in addition to rolling periods and calendar days, for records up to one year old. Its output is source-linked and verified before display. Nothing is sent to a server, chat remains device-only, and no canned answer is introduced.

Questions about the selected child's present state use the record-backed path. Answers describe what the app has logged and distinguish a running timer from the child's physical state. General parenting questions remain available.
