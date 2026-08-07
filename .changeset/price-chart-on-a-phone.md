---
"ftw": patch
---

The price chart is shorter in the FTW app on a phone. It kept the shape the dashboard uses, which is right for a page about prices and wrong for the app's Plan screen, where a sentence, the mode choice and the hour-by-hour timeline share the screen with it: the chart alone painted 247 px of a 375×812 phone, so nothing else was ever on screen with it. It now paints 151 px, and the whole price block has gone from 57 % of the viewport to 45 %. Only the viewBox height moves — the rendered scale comes from the width — so the axis figures, the NOW marker and the peak and low markers come out at exactly the size they did before. `fed` is what tells the app apart from the dashboard, and the dashboard's Energy tab is unchanged at every width.

Fixed for both: the bottom y-axis figure lost its leading zero on a phone, rendering "0.00 ö" as ".00 ö". Each label is anchored just inside the left gutter and grows leftwards, and the gutter was a fixed 84 units while the phone's axis font is nearly three times the desktop one — so the widest label ran past the edge of the SVG and the browser clipped it. Even "335 ö" lost a hairline. The gutter is now measured from the labels the chart is about to write, which also covers the currencies whose unit is three characters wide.
