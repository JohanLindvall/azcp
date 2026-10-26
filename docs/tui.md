# Two-pane file manager

![The two-pane file manager browsing a local mount and nested virtual containers](assets/commander.png)

Start the Norton Commander-style interface explicitly:

```sh
azcp --tui
azcp --tui ./photos azure://myaccount/backup
azcp --tui --create-container . azure://myaccount
```

The optional operands set the left and right starting directories; each defaults
to the current directory. Local folders, Azure accounts, containers and virtual
directory prefixes all work. Ordinary `azcp SOURCE DEST` commands keep their
`cp` behavior, and an invocation without operands still reports a missing
operand. Both stdin and stderr must be terminals.

The interface uses the existing copy engine, credentials, parallel transfers
and retry policy. Azure sign-in instructions appear beneath the panes and in
the message viewer. The same credential and connection pool are reused for the
session. Listings and operations run in the background so cancellation remains
available during a slow request.

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
| F2 / `v` | Open virtual containers |
| F3 | Read-only text preview, limited to the first 64 KiB |
| F4 / `g` | Go to a path, Azure URL or virtual location |
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
terminals that reserve those keys.

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
name. Filesystem roots, Azure accounts and Azure containers themselves cannot
be moved or deleted; open them and select their contents instead.

F7 creates Azure directory markers so empty directories remain visible.
Creating a container at an account root requires `--create-container`.

## Virtual containers

A virtual container can contain other virtual containers, as well as named
local and Azure folders. This creates a navigable hierarchy without copying
data or creating containers in Azure. For example, `project` can contain a
local checkout and an `environments` container, which in turn contains
`development` and `production`. Mounts at the leaves point to real storage.

Create `azcp/tui.json` under the operating system's user configuration directory
(`$XDG_CONFIG_HOME` or `~/.config` on Linux, `~/Library/Application Support` on
macOS, `%AppData%` on Windows):

```json
{
  "containers": {
    "project": {
      "checkout": "/home/me/src/project",
      "environments": {
        "development": {
          "uploads": "azure://devaccount/incoming/project"
        },
        "production": {
          "uploads": "azure://prodaccount/incoming/project",
          "archive": "azure://archiveaccount/backups/project"
        }
      }
    }
  }
}
```

Use a local path suitable for your machine. Relative local paths are resolved
from the configuration file's directory. Container and mount names accept
letters, digits, dots, underscores and hyphens, excluding `.` and `..`.
An object creates a virtual container; a string creates a mount. Containers
can be empty and can nest up to 32 levels. The panes label them `<VIRTUAL>` and
label backing locations `<MOUNT>`; Enter and Backspace navigate the hierarchy.
Within an Azure mount, ordinary blob prefixes provide further virtual
directories, which F7 can create at any depth.

Open the collection with F2, or launch directly into it:

```sh
azcp --tui virtual://project .
azcp --tui --tui-config ./folders.json virtual://project/checkout virtual://project/environments/production/uploads
```

The configuration is read at startup. Virtual containers and mount references
cannot be deleted or moved from the interface. Enter a mount to work on its
real contents; those actions affect the backing local directory or Azure
location. A mount can also be copied as its underlying directory. Destinations
must be real folders or mounted folders, not the collection itself.

The usual Azure credential discovery works for mounts. A SAS in a configured
URL is passed to storage but redacted on screen; prefer your existing sign-in
or environment credentials to saving tokens in the configuration file.

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
