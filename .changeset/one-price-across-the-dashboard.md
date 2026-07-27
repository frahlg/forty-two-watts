---
"ftw": patch
---

The dashboard now shows one price. The Electricity prices card applied VAT to bare spot and labelled the result "incl. VAT", leaving the grid tariff out — on a 70 öre/kWh tariff it read 21 öre for a slot the Plan chart correctly priced at 109. Its toggle is now Total / Spot, where Total is (spot + grid tariff) × (1 + VAT), the same arithmetic as the tariff engine and the Plan tooltip, and hovering a slot breaks the total into its three parts. The stored preference carries over, and the header names the tariff when there is one.

The Plan chart's price band no longer overflows. It drew its bars from the plan's actions — the full horizon, ML-forecast slots included — but scaled them off published prices only, so before the day-ahead release every forecast slot sat above the known maximum: bars were drawn past the top of the band and over the mode strip, and all of them landed above the p75 threshold, painting tomorrow as one solid expensive wall. The scale and the cheap/expensive thresholds now come from the same rows that get drawn, and the band is clipped as a backstop. Predicted slots are marked with a cap on top of the bar instead of a dashed frame around it, which at 15-minute resolution had merged into a hatch that hid the prices behind it.

Overview gains a price outlook: what a kWh costs now, the cheapest and priciest two-hour window ahead, and a strip of each slot's distance from the 24-hour average. Plotting distance rather than the total keeps the fixed tariff — often four fifths of the bill — from flattening every bar to the same height. Direction against the average carries cheap-versus-dear, so the strip still reads in greyscale and for colour-blind readers, who cannot separate this theme's green and red.
