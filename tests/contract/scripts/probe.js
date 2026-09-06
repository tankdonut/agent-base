// Contract canary for the trigger-script surface: stages A–C must keep
// seeding `--trigger-script /opt/agent/scripts/probe.js` against the
// real CLI (stage B polices the flag against `cron add|edit --help`).
json({ fire: false })
