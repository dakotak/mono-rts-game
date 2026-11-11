# dockview (vendored)

The admin workspace's docking layout: [dockview](https://dockview.dev)
**8.6.2** (MIT, see `LICENCE.md`), the framework-free JavaScript build
`dist/dockview.min.js` from the npm package `dockview@8.6.2`, unmodified.
It defines the global `dockview` and injects its own stylesheet.

Upgrading: replace `dockview.min.js` and `LICENCE.md` with the same files
from a newer `npm pack dockview@<version>`, update the version here, and
check the admin workspace (`docs/operations/administration.md`). Saved
layouts are versioned (`LAYOUT_VERSION` in `admin/workspace.js`); bump it
if the serialized format changes.
