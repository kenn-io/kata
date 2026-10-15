package main

import (
	"bytes"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func renderHelpForTest(t *testing.T, cmd *cobra.Command) string {
	t.Helper()
	var output bytes.Buffer
	cmd.SetOut(&output)
	require.NoError(t, cmd.Help())
	return output.String()
}

func TestNotificationCommandsRenderAgentHelp(t *testing.T) {
	notify := newNotifyCmd()
	notifyHelp := renderHelpForTest(t, notify)
	require.Equal(t, `Put a request in the recipient's inbox. --to takes an address: <actor> or
<actor>/<teammate>. --message is required unless --clear. One request per
issue and recipient; a new one replaces the old. Clear it after handling.
--re <comment> asks the recipient to answer that comment; their reply with
comment --reply clears the request, and wait --until reply wakes you.
--broadcast replaces --to: it asks this issue's participants and those of
its open children (at most 50); --teammates adds their teammate inboxes.`, notify.Long)
	require.Equal(t, `  kata notify abc4 --to coordinator --message "Need a decision on the schema" --agent
  kata notify abc4 --to coordinator/teammate-1 --message "Please re-run the tests" --agent
  kata notify abc4 --to reviewer/teammate-2 --re c:abc123 --message "Please confirm or refute this finding" --agent
  kata notify abc4 --broadcast --message "Schema changed; re-check your branches" --agent
  kata notify abc4 --to coordinator/teammate-1 --clear --agent`, notify.Example)
	require.Contains(t, notifyHelp, "Put a request in the recipient's inbox.")
	require.Contains(t, notifyHelp, "--broadcast replaces --to:")
	require.Contains(t, notifyHelp, "kata notify abc4 --to coordinator --message")
	require.Contains(t, notifyHelp, "kata notify abc4 --broadcast --message")
	require.Equal(t, "ask an actor or teammate to look at an issue (or --clear)", notify.Short)
	require.Equal(t, "recipient: <actor> or <actor>/<teammate> (required unless --broadcast)", notify.Flag("to").Usage)
	require.Equal(t, "why their attention is needed (required unless --clear; max 1024 bytes)", notify.Flag("message").Usage)

	inbox := newInboxCmd()
	inboxHelp := renderHelpForTest(t, inbox)
	require.Equal(t, `List open requests addressed exactly to --for (or KATA_INBOX_USER):
<actor> or <actor>/<teammate>. "coordinator" does not include
"coordinator/*". Bound project by default; --all reads every active project
and prints qualified refs. --context emits bounded context for a harness.
--context conflicts with --agent, --json, or --format; omit output selectors.
Reading does not clear a request; clear it with kata notify --clear.

A row with re=<comment> asks you to answer that comment; answering with
comment --reply <comment> on that issue clears it. A row with kind= reports
a reply to one of your comments; broadcast=true marks a notify --broadcast.`, inbox.Long)
	require.Equal(t, `  kata inbox --for coordinator/teammate-1 --agent
  kata inbox --for coordinator --all --agent
  kata inbox --for coordinator/teammate-1 --context`, inbox.Example)
	require.Contains(t, inboxHelp, "A row with re=<comment> asks you to answer that comment;")
	require.Contains(t, inboxHelp, "kata inbox --for coordinator/teammate-1 --agent")

	wait := newWaitCmd()
	waitHelp := renderHelpForTest(t, wait)
	require.Equal(t, `  kata wait abc4 d4ex j7m2 --until attention --any --timeout 30m --agent
  kata wait abc4 --until closed --timeout 2h --json
  kata wait abc4 --until reply --timeout 30m --agent`, wait.Example)
	require.Contains(t, waitHelp, "Block until one or more issues reach a target condition.")
	require.Contains(t, waitHelp, "kata wait abc4 --until reply --timeout 30m --agent")
}
