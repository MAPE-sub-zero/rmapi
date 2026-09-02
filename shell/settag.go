package shell

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/abiosoft/ishell"
	"github.com/juruen/rmapi/api/sync15"
	"github.com/ogier/pflag"
)

// settagErrorOutput is the JSON error object settag prints on stdout in
// --json mode. The shell's normal exit path (c.Err) is unchanged; this is
// additional, not instead of.
type settagErrorOutput struct {
	Error            string                  `json:"error"`
	Kind             string                  `json:"kind"`
	DocumentID       string                  `json:"documentId"`
	ExpectedRevision string                  `json:"expectedRevision,omitempty"`
	ActualRevision   string                  `json:"actualRevision,omitempty"`
	Result           *sync15.TagUpdateResult `json:"result"`
}

// settagErrorKind classifies err by errors.As against the sentinel-carrying
// error types UpdateDocumentTags can return, for the "kind" field of the
// JSON error object.
func settagErrorKind(err error) (kind, expected, actual string) {
	var stale *sync15.StaleRevisionError
	if errors.As(err, &stale) {
		return "stale_revision", stale.Expected, stale.Actual
	}
	var superseded *sync15.SupersededError
	if errors.As(err, &superseded) {
		return "superseded", superseded.ExpectedRevision, superseded.ActualRevision
	}
	var notCommitted *sync15.NotCommittedError
	if errors.As(err, &notCommitted) {
		return "not_committed", notCommitted.ExpectedRevision, notCommitted.ActualRevision
	}
	return "error", "", ""
}

// printSettagErrorJSON prints the structured JSON error object for a failed
// settag call. It is best-effort: a marshal failure here must not hide the
// original error, which the caller still reports via c.Err.
func printSettagErrorJSON(c *ishell.Context, docId string, err error, result *sync15.TagUpdateResult) {
	kind, expected, actual := settagErrorKind(err)
	out := settagErrorOutput{
		Error:            err.Error(),
		Kind:             kind,
		DocumentID:       docId,
		ExpectedRevision: expected,
		ActualRevision:   actual,
		Result:           result,
	}
	b, mErr := json.MarshalIndent(out, "", "  ")
	if mErr != nil {
		return
	}
	c.Println(string(b))
}

// parseTags parses a comma-separated tag string into a slice of non-empty trimmed tag names.
func parseTags(tagsStr string) []string {
	if strings.TrimSpace(tagsStr) == "" {
		return []string{}
	}
	parts := strings.Split(tagsStr, ",")
	tags := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			tags = append(tags, trimmed)
		}
	}
	return tags
}

// settagArgs is the parsed, shell-independent form of a settag invocation.
type settagArgs struct {
	Op               sync15.TagOp
	Tags             []string
	Path             string
	ExpectedRevision string
}

// settagFlags holds the raw flag values parsed by newSettagFlagSet.
type settagFlags struct {
	add, remove bool
	ifRevision  string
}

const settagUsage = "usage: settag [--add|--remove] [--if-revision=<hash>] <path> <tag1,tag2...>"

func newSettagFlagSet() (*pflag.FlagSet, *settagFlags) {
	f := &settagFlags{}
	fs := pflag.NewFlagSet("settag", pflag.ContinueOnError)
	fs.BoolVar(&f.add, "add", false, "add the supplied tags, leaving existing ones in place")
	fs.BoolVar(&f.remove, "remove", false, "remove the supplied tags, leaving the rest in place")
	fs.StringVar(&f.ifRevision, "if-revision", "", "only write if the document is currently at this revision hash")
	return fs, f
}

// resolveSettagArgs validates the parsed flags and positional arguments. It
// does not touch the filesystem or the API, so it can be unit-tested without
// a shell.
func resolveSettagArgs(f *settagFlags, positional []string) (*settagArgs, error) {
	op := sync15.TagOpReplace
	if f.add && f.remove {
		return nil, errors.New("choose one of --add, --remove")
	}
	if f.add {
		op = sync15.TagOpAdd
	}
	if f.remove {
		op = sync15.TagOpRemove
	}

	if len(positional) != 2 {
		return nil, errors.New(settagUsage)
	}

	return &settagArgs{
		Op:               op,
		Tags:             parseTags(positional[1]),
		Path:             positional[0],
		ExpectedRevision: f.ifRevision,
	}, nil
}

// parseSettagArgs parses settag's flags and positional arguments. It is the
// test seam: it builds the flag set, parses args, then delegates to
// resolveSettagArgs.
func parseSettagArgs(args []string) (*settagArgs, error) {
	fs, f := newSettagFlagSet()
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	return resolveSettagArgs(f, fs.Args())
}

func settagCmd(ctx *ShellCtxt) *ishell.Cmd {
	longHelp := `Usage: settag [--add|--remove] [--if-revision=<hash>] <path> <tag1,tag2...>

Tags are comma-separated. With no operation flag the document's tags are replaced.`

	return &ishell.Cmd{
		Name:      "settag",
		Help:      "set document tags",
		Completer: createEntryCompleter(ctx),
		LongHelp:  longHelp,
		Func: func(c *ishell.Context) {
			flagSet, flags := newSettagFlagSet()
			if !processFlagSet(flagSet, longHelp, c.Args, c) {
				return
			}

			args, err := resolveSettagArgs(flags, flagSet.Args())
			if err != nil {
				c.Err(err)
				return
			}

			node, err := ctx.api.Filetree().NodeByPath(args.Path, ctx.node)
			if err != nil {
				c.Err(errors.New("file doesn't exist"))
				return
			}

			if node.IsRoot() {
				c.Err(errors.New("cannot set tags on root"))
				return
			}

			if node.IsDirectory() {
				c.Err(errors.New("cannot set tags on a folder"))
				return
			}

			result, err := ctx.api.UpdateDocumentTags(
				node.Document.ID, args.Op, args.Tags, args.ExpectedRevision)
			if err != nil {
				if ctx.JSONOutput {
					printSettagErrorJSON(c, node.Document.ID, err, result)
				}
				c.Err(fmt.Errorf("failed to set tags: %w", err))
				return
			}

			node.Document.Tags = result.AfterTags

			if ctx.JSONOutput {
				out, err := json.MarshalIndent(result, "", "  ")
				if err != nil {
					c.Err(fmt.Errorf("failed to marshal result: %w", err))
					return
				}
				c.Println(string(out))
			} else if !result.Changed {
				c.Printf("unchanged: %s already has tags [%s]\n", args.Path, strings.Join(result.AfterTags, ", "))
			} else {
				c.Printf("%s: %s\n", result.Operation, args.Path)
				c.Printf("  tags: [%s] -> [%s]\n", strings.Join(result.BeforeTags, ", "), strings.Join(result.AfterTags, ", "))
				c.Printf("  revision: %s -> %s\n", result.BeforeRevision, result.AfterRevision)
				c.Printf("  page tags preserved: %d\n", result.PageTagCount)
			}

			if result.Changed {
				err = ctx.api.SyncComplete()
				if err != nil {
					c.Err(fmt.Errorf("cannot notify: %w", err))
				}
			}
		},
	}
}
