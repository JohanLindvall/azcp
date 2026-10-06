# Security

`azcp` handles credentials — SAS tokens, account keys, bearer tokens — and
writes files where it is told to. A flaw in either is a security problem, and
this is where to report one.

## Reporting a vulnerability

Please do not open a public issue. Report it privately instead, through
GitHub:

**<https://github.com/JohanLindvall/azcp/security/advisories/new>**

That opens a draft advisory that only you and the maintainer can see. Include
the version (`azcp --version`), the command line with any credentials removed,
what happened and what you expected, and whatever is needed to reproduce it.
You will hear back, and a fix — or the reason there is none — comes before any
public disclosure, which is coordinated with you. Credit is given unless you
would rather it was not.

## What counts

Anything that lets a credential leak — into a log, an error message, a process
listing or a file — or lets a copy read or write outside what the command line
named: a blob name that escapes the destination directory, a symbolic link
followed where it should not have been, a `--delete` that removes more than
the source lacks. A checksum or integrity failure that goes unreported belongs
here too.

Being able to do what the credential's own permissions allow does not: `azcp`
can only do what the account it was given can.

## Supported versions

Fixes go into the next release; there are no maintenance branches. Run the
latest release, which is what the install script fetches.
