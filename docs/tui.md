# Two-pane file manager

![The file manager browsing local folders and nested Azure blob directories](assets/commander.png)

Start the Norton Commander-style interface explicitly:

```sh
azcp --tui
azcp --tui ./photos azure://myaccount/backup
azcp --tui --create-container . azure://myaccount
```

The optional operands set the left and right starting directories; each defaults
to the current directory. Ordinary `azcp SOURCE DEST` commands keep their `cp`
behavior, and an invocation without operands still reports a missing operand.
Both stdin and stderr must be terminals.

## Azure as a directory tree

An Azure account opens as a directory of storage containers. Open a container
to see its blobs and virtual directories. Blob prefixes become directories at
as many levels as the blob names contain:

```text
azure://myaccount/
└── backups/                    storage container
    └── projects/               virtual directory
        └── website/            virtual directory
            ├── assets/         virtual directory
            │   └── logo.png    blob
            └── README.md       blob
```

For example, `projects/website/assets/logo.png` is the blob's key within the
`backups` container. The panes expose each component as a folder, so Enter,
Backspace, selection, recursive copy, move and delete work like they do on a
local directory tree. No additional configuration is needed. Either pane can
point to local storage or any accessible Azure account, container or prefix.
If a blob shares a name with a prefix, both appear as separate entries (`name`
and `name/`), so neither object is hidden.

F7 creates a directory at the current level, including inside an existing
virtual directory. Azure empty directories are represented by zero-byte marker
blobs ending in `/`, so they survive refreshes and copies. Creating an actual
storage container at the account root requires `--create-container`.

The interface uses the existing copy engine, credentials, parallel transfers
and retry policy. Azure sign-in instructions appear beneath the panes and in
the message viewer. The same credentials and connection pool are reused for
the session. Listings and operations run in the background so cancellation
remains available during a slow request. SAS tokens stay redacted on screen.

## Keys

| Key | Action |
| --- | --- |
| Tab | Switch pane |
| Up / Down, Home / End, Page Up / Down | Move the cursor |
| Enter / Right | Enter a directory or preview a file |
| Backspace / Left | Go to the parent |
| Space / Insert | Toggle selection and advance |
| `*` | Invert selection |
| F1 / `?` | Help |
| F2 / `v` | Go to the current filesystem or Azure container root |
| F3 | Read-only text preview, limited to the first 64 KiB |
| F4 / `g` | Go to a local path or Azure URL |
| F5 / `c` | Copy selected entries |
| F6 / `m` | Move selected entries |
| F7 / `n` | Create a directory |
| F8 / `d` / Delete | Permanently delete selected entries |
| F9 / `l` | Show messages and sign-in instructions |
| Ctrl-R / `r` | Refresh both panes |
| F10 / `q` | Quit; cancel active work and wait for it to stop |
| Esc / Ctrl-C during an operation | Cancel the operation |

With no marked entries, actions use the entry under the cursor. The parent
entry cannot be selected. Function keys also have letter alternatives for
terminals that reserve those keys. F2 stays inside the current Azure container,
so returning to its root works with a container-scoped SAS.

Copy and move prompt for a destination directory. Leave the input empty to use
the other pane, or enter another existing directory. A relative destination is
resolved from the other pane. Use F7 to create a directory first. Path prompts
accept Backspace and Ctrl-U to clear the input.

Copy, move and delete then display a confirmation. Press **y** to proceed;
Enter, **n** and Esc cancel. Copies replace existing destination files and merge
directory contents. Moves copy first and remove sources only after every copy
succeeds, nothing was skipped, the operation was not cancelled, and a fresh
source inventory agrees with the original. A failed copy leaves all move
sources in place, including files already copied successfully.

Moves are copy-and-delete operations, including between local directories;
they are not atomic renames. If source removal fails or is cancelled after it
starts, some sources may already be removed, while their destination copies
remain. Completed changes are not rolled back. Avoid editing files involved
in an active move. Azure blob removals use ETag conditions so a concurrent
replacement is retained.

Recursive deletion lists the entire selection before removing anything and
does not follow local symlinks. Directory deletion removes only that Azure
prefix and its marker, preserving a distinct blob with the same unsuffixed
name. Filesystem roots, Azure accounts and Azure storage containers themselves
cannot be moved or deleted; open them and select their contents instead.

## Transfer options

`--tui` accepts authentication and connection settings, `--jobs`,
`--part-size`, `--part-concurrency`, retry settings, `--bwlimit`, `--timeout`,
`--create-container`, `--put-md5`, `--check-md5`, `--resume`, log settings and
`--dry-run`. Dry runs permit exploration and confirmation dialogs but do not
copy, move, create or delete files.

Copy-policy flags such as `-i`, `-n`, `-u`, `--exclude`, `--compress`,
`--delete`, and JSON output are rejected with `--tui`. The interface supplies
its own selection and confirmation policy; rejecting these combinations keeps
a move from silently skipping files before removing sources. Use ordinary
`azcp` commands for those policies.
