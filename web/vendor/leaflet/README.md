# Leaflet, vendored

Leaflet 1.9.4, BSD-2-Clause. See `LICENSE`.

Vendored rather than pulled from a CDN, for the same reason as `/vendor/three/`
and `/vendor/ace/`: the box UI must not execute third-party JS from a CDN, and
the weather map has to load even when the gateway cannot reach the internet.

Only the browser build and the files it needs, taken from the `leaflet` npm
package (`dist/`):

| File | Why |
|---|---|
| `leaflet.js` | the map library (UMD/browser build, not ESM) |
| `leaflet.css` | default controls and marker styles |
| `images/marker-icon.png` | default marker |
| `images/marker-icon-2x.png` | retina marker |
| `images/marker-shadow.png` | default marker shadow |
| `images/layers.png` | layers-control toggle |
| `images/layers-2x.png` | retina layers-control toggle |

## Updating

    curl -sL https://registry.npmjs.org/leaflet/-/leaflet-<version>.tgz -o leaflet.tgz
    tar xzf leaflet.tgz package/dist/leaflet.js package/dist/leaflet.css package/dist/images package/LICENSE
    mkdir -p web/vendor/leaflet/images
    cp package/dist/leaflet.js package/dist/leaflet.css package/LICENSE web/vendor/leaflet/
    cp package/dist/images/* web/vendor/leaflet/images/

Tarball sha256 for 1.9.4:
`84c65a256e50657896f54c33bd857b6849ebe94c817803be818bf32a3dde0b77`
