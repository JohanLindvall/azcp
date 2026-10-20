# Repository presentation

Keep the first visit focused on what azcp does, a working first copy, and where
to get help. The README introduces the tool; `docs/` carries the reference.
This follows [GitHub's README guidance](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-readmes).

## GitHub settings

After the documentation is published, use this short **About** description:

> cp for Azure Blob Storage. Copy local files and blobs with familiar flags, remote wildcards, parallel transfers, resume and compression.

The repository already has relevant topics. Keep them focused on what the
tool does: `azure`, `azure-blob-storage`, `cli`, `cp`, `file-transfer`, `golang`,
`globbing` and `azcopy-alternative` are useful examples. Topics help readers
find related projects; avoid unrelated trend tags. See
[GitHub's topic guidance](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/classifying-your-repository-with-topics).

Upload [social-preview.png](../docs/assets/social-preview.png) in the
[repository settings](https://github.com/JohanLindvall/azcp/settings), under
**Social preview → Edit → Upload an image**. The image is 1280 × 640 pixels,
under 1 MB, and uses an opaque background for predictable link previews.
Committing the image does not activate it as the repository's preview;
GitHub requires the settings upload. See
[the social-preview instructions](https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/customizing-your-repositorys-social-media-preview).

The editable artwork lives in
[banner.svg](../docs/assets/banner.svg) and
[social-preview.svg](../docs/assets/social-preview.svg).
After changing the latter, export it to `social-preview.png` at 1280 × 640 and
check the text before uploading it. The artwork uses Ubuntu Sans and DejaVu
Sans Mono, with fallback fonts, and has no external assets.

## Share something people can try

Lead with a specific task and a working command. For example:

```text
azcp brings familiar cp commands to Azure Blob Storage:

  azcp -r ./photos azure://myaccount/backup/

It copies local files too, expands remote ** patterns, and supports parallel
transfers, resume and compression. Available for Linux, macOS and Windows.

The quick start includes a local emulator walkthrough, so you can try blob
transfers without an Azure account. Feedback on real copy workflows is welcome.

https://github.com/JohanLindvall/azcp
```

Adapt the example to the audience: collecting logs, publishing build artifacts,
or moving a dataset. Explain the account/container prerequisites alongside a
cloud command. Share where that workflow is relevant and follow the
community's posting rules.

Use the comparison guide when discussing performance. Retain the tool
versions, workload and measurement limits; request counts from Azurite are
not a cloud throughput benchmark.

## Give people a reason to return

- Write release summaries around changes users can try. The release workflow
  appends GitHub's generated change list and contributor credits to the install
  notes; add context when a release has a substantial feature.
- Keep small, clearly scoped issues available. Apply `good first issue` only
  when the expected result and a useful starting point are described.
- Turn recurring support questions into examples, then link those examples
  from the issue form or quick start.
- Keep the README's single star invitation unobtrusive. Encourage release
  subscriptions and contributions as well.

Review the result through completed first copies, useful bug reports,
contributions and repeat users, alongside traffic and stars.
