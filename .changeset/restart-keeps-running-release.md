---
"ftw": patch
---

Restart keeps the release that is running. Compose resolves `${FTW_IMAGE_TAG:-latest}` afresh on every recreate, and the pin in `.env` or the registry's `latest` can name another release: on a beta box one press of Restart replaced v2.14.0-beta.1 with v2.0.0-beta.2, and the loadpoint added since was gone with it. The updater now reads the tag of the running container and pins it for the recreate, and Core names its own release when it asks for a restart, so an older updater keeps the release too. A build without a release tag still lets compose resolve the image.
