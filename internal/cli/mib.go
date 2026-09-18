package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/upstacked/cli/internal/errs"
	"github.com/upstacked/cli/internal/mib"
	"github.com/upstacked/cli/internal/output"
)

func newMIBCmd(app *App) *cobra.Command {
	c := &cobra.Command{
		Use:   "mib",
		Short: "Local MIB repository: sync, search and resolve OIDs",
		Long: `Cache public MIB repositories locally and look objects up in them.

An SNMP monitoring item is a set of OIDs plus the meaning of what comes back.
The OIDs live in MIB files, and the useful ones are not the scalars in
SNMPv2-MIB - they are the vendor tables, which no amount of guessing will
produce. So the cache is the reference: 'search' finds a name, 'walk' lists a
subtree the way the device will return it, and 'show' gives the numeric OID,
its syntax and what the vendor says it means.

Nothing here touches a device or the Upstacked API. Resolution is offline and
the index is a plain TSV file, so grep works on it too - which matters when a
question does not fit the flags.

  ups mib sync                    # clone or update the repositories
  ups mib search cpmCPUTotal      # find an object by name
  ups mib walk ifXTable           # list a subtree
  ups mib show ifHCInOctets       # numeric OID, syntax, description`,
	}
	c.AddCommand(
		newMIBSyncCmd(app), newMIBStatusCmd(app), newMIBPathCmd(app),
		newMIBSearchCmd(app), newMIBWalkCmd(app), newMIBShowCmd(app),
	)
	return c
}

func mibStore() (*mib.Store, error) {
	s, err := mib.NewStore("")
	if err != nil {
		return nil, errs.General("%v", err)
	}
	return s, nil
}

func newMIBSyncCmd(app *App) *cobra.Command {
	var repos []string
	var reindexOnly bool
	c := &cobra.Command{
		Use:   "sync",
		Short: "Clone or update the MIB repositories and rebuild the index",
		Long: `Fetch the MIB repositories into the local cache and index them.

Clones are shallow and single-branch: the files are what matter, the history
is not. Re-running updates in place.

By default this syncs every known repository:

  librenms  https://github.com/librenms/librenms-mibs
  cisco     https://github.com/cisco/cisco-mibs

--repo takes one of those names, or any git URL, and can be repeated. Pass
--reindex to rebuild the index from what is already cached without touching
the network - which is what you want after adding MIB files by hand.

Indexing resolves every object's numeric OID by walking its parent chain
across all cached MIBs at once. An object whose parent is defined in a MIB
the cache does not carry is indexed with no OID rather than a guessed one,
because a guessed OID polls the wrong thing.`,
		Example: `  ups mib sync
  ups mib sync --repo cisco
  ups mib sync --repo https://github.com/example/vendor-mibs.git
  ups mib sync --reindex`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := mibStore()
			if err != nil {
				return err
			}
			if app.DryRun {
				app.Printer.Infof("dry-run: would sync into %s", store.Dir)
				return nil
			}

			if !reindexOnly {
				want := mib.DefaultRepos
				if len(repos) > 0 {
					want = nil
					for _, r := range repos {
						want = append(want, mib.RepoByName(r))
					}
				}
				for _, r := range want {
					var st mib.RepoState
					err := app.Spin("Syncing "+r.Name, func() error {
						var e error
						st, e = store.Fetch(r, nil)
						return e
					})
					if err != nil {
						return errs.General("%v", err).
							WithHint("the repositories are large; check network access, or pass --reindex to work with what is already cached")
					}
					app.Printer.Infof("%s %s at %s", app.Sym().OK, r.Name, dash(st.Revision))
				}
			}

			var count int
			if err := app.Spin("Indexing MIBs", func() error {
				var e error
				count, e = store.Reindex()
				return e
			}); err != nil {
				return errs.General("cannot index the MIB cache: %v", err)
			}
			app.Printer.Infof("%s Indexed %d objects into %s", app.Sym().OK, count, store.IndexPath())
			return nil
		},
	}
	c.Flags().StringSliceVar(&repos, "repo", nil, "repository name or git URL (repeatable; default: all known)")
	c.Flags().BoolVar(&reindexOnly, "reindex", false, "rebuild the index from the existing cache without fetching")
	return c
}

func newMIBPathCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the MIB cache directory",
		Long: `Print the MIB cache directory.

The index under it is TSV - name, oid, kind, syntax, access, module, file,
description - so a question the search flags do not express can be answered
with grep instead.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := mibStore()
			if err != nil {
				return err
			}
			if app.AsJSON {
				return app.Printer.JSON(map[string]string{
					"dir": store.Dir, "index": store.IndexPath(),
				})
			}
			app.Printer.Printf("%s", store.Dir)
			return nil
		},
	}
}

func newMIBStatusCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which MIB repositories are cached and how current they are",
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := mibStore()
			if err != nil {
				return err
			}
			cached := store.Cached()
			t := &output.Table{
				Columns: []string{"REPO", "REVISION", "SYNCED", "URL"},
				Empty:   "No MIB repositories cached. Run: ups mib sync",
			}
			for _, st := range cached {
				raw, _ := json.Marshal(map[string]any{
					"name": st.Name, "revision": st.Revision,
					"synced": st.Synced.Format(time.RFC3339), "url": st.URL,
				})
				t.Add(st.Name, raw, st.Name, dash(st.Revision),
					st.Synced.Format("2006-01-02"), dash(st.URL))
			}
			if err := app.Printer.Print(t); err != nil {
				return err
			}
			if _, _, err := store.Search(mib.Query{Limit: 1}); errors.Is(err, mib.ErrNoIndex) {
				fmt.Fprintf(app.Stderr, "  %s no index yet, so search and show will find nothing: ups mib sync\n",
					app.Theme().Yellow.Apply(app.Sym().Warn))
			}
			return nil
		},
	}
}

func newMIBSearchCmd(app *App) *cobra.Command {
	var module, oid string
	var describe bool
	c := &cobra.Command{
		Use:   "search <text>",
		Short: "Find MIB objects by name",
		Long: `Search the indexed MIB objects.

Matching is a case-insensitive substring of the object name; --describe
widens it to the vendor's DESCRIPTION text, which is how you find the right
object when you know the measurement but not its name.`,
		Example: `  ups mib search cpmCPUTotal
  ups mib search temperature --describe --module CISCO-ENVMON-MIB
  ups mib search "" --oid 1.3.6.1.2.1.31.1.1.1`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := ""
			if len(args) == 1 {
				text = args[0]
			}
			if text == "" && oid == "" && module == "" {
				return errs.Usage("give some text to search for, or --oid / --module")
			}
			return app.printMIBRows(mib.Query{
				Text: text, OID: oid, Module: module, Describe: describe, Limit: app.Limit,
			}, "No matching MIB objects.")
		},
	}
	c.Flags().StringVar(&module, "module", "", "restrict to one MIB module")
	c.Flags().StringVar(&oid, "oid", "", "restrict to a numeric OID subtree")
	c.Flags().BoolVar(&describe, "describe", false, "also match the DESCRIPTION text")
	return c
}

func newMIBWalkCmd(app *App) *cobra.Command {
	var module string
	c := &cobra.Command{
		Use:   "walk <name|oid>",
		Short: "List a MIB subtree in OID order",
		Long: `List everything under one node, in OID order.

This is the offline half of walking a device: it says what the subtree
contains and what each column means. It does not poll anything - to see what
a device actually returns, create the item and run
'ups monitoring item dry-run'.

Walking the table you intend to poll is the step that catches the common
mistake: taking a scalar when the device only populates the table, or taking
the table's index column instead of the counter.`,
		Example: `  ups mib walk ifXTable
  ups mib walk 1.3.6.1.2.1.31.1.1.1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := mibStore()
			if err != nil {
				return err
			}
			root := args[0]
			if !isNumericOID(root) {
				o, err := store.Lookup(root)
				if err != nil {
					return mibLookupError(err, root)
				}
				if o.OID == "" {
					return errs.General("%s has no resolvable OID in the cache, so its subtree cannot be walked", root).
						WithHint("the MIB that defines its parent is missing: ups mib sync")
				}
				root = o.OID
			}
			return app.printMIBRows(mib.Query{OID: root, Module: module, Limit: app.Limit},
				"Nothing under that OID in the cache.")
		},
	}
	c.Flags().StringVar(&module, "module", "", "restrict to one MIB module")
	return c
}

func newMIBShowCmd(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name|oid>",
		Short: "Show one MIB object's OID, syntax and description",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := mibStore()
			if err != nil {
				return err
			}
			o, err := store.Lookup(args[0])
			if err != nil {
				return mibLookupError(err, args[0])
			}
			raw, _ := json.Marshal(mibDoc(o))
			if err := app.Printer.Object(raw, [][2]string{
				{"Name", o.Name},
				{"OID", dash(o.OID)},
				{"Kind", dash(o.Kind)},
				{"Syntax", dash(o.Syntax)},
				{"Access", dash(o.Access)},
				{"Module", dash(o.Module)},
				{"File", dash(o.File)},
				{"Description", dash(o.Description)},
			}); err != nil {
				return err
			}
			if o.OID == "" && !app.AsJSON {
				fmt.Fprintf(app.Stderr, "  %s the OID could not be resolved: the MIB defining %s is not in the cache.\n",
					app.Theme().Yellow.Apply(app.Sym().Warn), dash(o.Module))
			}
			return nil
		},
	}
}

// printMIBRows renders a query result, saying so when the answer was capped.
func (a *App) printMIBRows(q mib.Query, empty string) error {
	store, err := mibStore()
	if err != nil {
		return err
	}
	objs, truncated, err := store.Search(q)
	if err != nil {
		return mibLookupError(err, "")
	}
	t := &output.Table{
		Columns:   []string{"NAME", "OID", "KIND", "SYNTAX", "MODULE"},
		Truncated: truncated,
		Empty:     empty,
	}
	for _, o := range objs {
		raw, _ := json.Marshal(mibDoc(o))
		t.Add(o.Name, raw, o.Name, dash(o.OID), dash(o.Kind),
			truncate(dash(o.Syntax), 28), dash(o.Module))
	}
	return a.Printer.Print(t)
}

func mibDoc(o mib.Object) map[string]string {
	return map[string]string{
		"name": o.Name, "oid": o.OID, "kind": o.Kind, "syntax": o.Syntax,
		"access": o.Access, "module": o.Module, "file": o.File,
		"description": o.Description,
	}
}

// mibLookupError turns a missing cache into the action that fixes it. An empty
// cache and an unknown object read identically otherwise, and they need
// different answers.
func mibLookupError(err error, what string) error {
	if errors.Is(err, mib.ErrNoIndex) {
		return errs.NotFound("the MIB cache has not been built yet").
			WithHint("run: ups mib sync")
	}
	if what != "" {
		return errs.NotFound("%v", err).
			WithHint("search for it: ups mib search %s   (or sync more repositories: ups mib sync)", what)
	}
	return errs.General("%v", err)
}

func isNumericOID(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
