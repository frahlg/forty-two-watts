---
"ftw": minor
---

New dashboard demand card for tariff-configured C&I sites: the running 30-min demand window (band, billed/unbilled, progress), the billing-cycle peak, utilization of the Notified Maximum Demand with near/over highlighting, and the demand cost so far. Self-fetching from `/api/demand`; on residential sites the endpoint 404s and the card removes itself, leaving the dashboard unchanged.
