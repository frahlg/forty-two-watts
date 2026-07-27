---
"ftw": minor
---

Suggest a driver change back to device-drivers from the gateway. One click opens a pre-filled issue carrying the driver, its version, where the running copy came from, the hash it was based on, and the edit as a diff. The gateway holds no GitHub token and needs none — GitHub accepts a pre-filled issue over a URL and the operator is already signed in to their own browser. The change travels as a diff rather than the whole file: drivers run to tens of kilobytes and a URL is limited to about eight, so sending the file meant the code never travelled at all.
