# ADR-008: Battery-aware sampling, and the measurements behind it

- **Status:** accepted
- **Date:** 2026-10-05

## Context

Slice 2 samples location at one fixed setting — `Accuracy.High`, a 10-second
interval, a 20-metre displacement filter — applied once when tracking starts
and never changed. `service.ts` has always described that as "the heavy
baseline", with the battery-aware slice adapting it and *measuring against
these numbers*. There were no numbers.

There are now. Four days of real tracking on the test device (OnePlus 5,
Android 10), including two runs down to 1%:

**Baseline: 5.58 % per hour, across 28.7 hours of measured discharging.**
Roughly **17.9 hours from a full charge** — adequate for a shift, not
comfortable, and well short of what a driver would want from a phone that is
also their phone.

Charging periods are excluded, and so are samples more than 30 minutes apart,
so the figure is drain while actually tracking rather than an average diluted by
idle time.

## The measurement that matters

Drain scales with **fix rate**, and this is the finding the slice turns on:

| Fix rate | Hours observed | Drain |
|---|---|---|
| 20-59 /h | 13 | 2.78 %/h |
| 60-119 /h | 4 | 5.80 %/h |
| 120+ /h | 11 | 9.07 %/h |

More fixes arrive while driving, so that table alone cannot separate "sampling
costs battery" from "driving costs battery". Restricting to **stationary hours
only**, where no driving fixes occurred at all:

| Fix rate (stationary only) | Hours | Drain |
|---|---|---|
| ~25 /h | 14 | **2.86 %/h** |
| ~137 /h | 3 | **11.43 %/h** |

Five times the fixes, four times the drain, with driving eliminated as a cause.
Sampling rate is the lever.

## Decision

**Adapt the sampling rate to motion state and charge level**, keyed primarily
on motion, with charge level tightening the tiers as the battery falls.

**Hysteresis is mandatory, not a refinement.** Changing `LocationTaskOptions`
means calling `startLocationUpdatesAsync` again, which restarts the location
subscription. Switching tiers on every battery tick or every speed wobble would
restart it constantly, and the restarts plausibly cost more than the sampling
saves. A tier change therefore requires both a threshold crossing *and* a
minimum dwell in the new state.

**A floor that charge level cannot lower.** A nearly flat battery must still
record enough to reconstruct a route, because a shift that produces unusable
data is worse than one that produces none — it looks like evidence.

**The fidelity cost gets measured, not assumed.** `stop_event_matches` currently
shows the device and server detectors agreeing to **5.7 s and 8.0 m** at the
present sampling rate. Sampling less should make that worse. The slice is only
done when the trade is stated as a number: drain fell by X%, agreement degraded
from 5.7 s to Y.

## Measured outcome (2026-10-17)

Eleven days of tracking with the tiers live, against the pre-slice baseline:

| | Drain | From full |
|---|---|---|
| Before 2d, fixed rate | 5.56 %/h | 18.0 h |
| After 2d, tiered | **3.06 %/h** | **32.7 h** |

Controlled for usage mix by matching on fix rate, because more driving means
both more fixes and more drain. The improvement survives:

| Period | Fixes/h | Drain |
|---|---|---|
| Before, high rate | 146 | 8.26 %/h |
| After, high rate | 168 | 4.26 %/h |
| Before, low rate | 26 | 3.03 %/h |
| After, low rate | 31 | 2.29 %/h |

Drain roughly halved in both bands *at slightly higher fix rates*, so this is
not the usage pattern.

**The mechanism is not the one this ADR predicted.** Delivery rate barely
moved — a stationary device averaged 52.3 s between fixes before and 52.1 s
after — because `distanceInterval: 20` was already suppressing most stationary
deliveries. The idle tier's 60 s interval is therefore close to a no-op against
what the active tier already did while parked.

So the saving almost certainly comes from `distanceInterval` widening 20 m to
50 m, which changes how aggressively the fused provider duty-cycles GPS, rather
than from delivering fewer fixes. Right outcome, wrong reason: the decision was
argued from fix *count* and the lever turned out to be the displacement filter.
That matters for tuning — raising `timeIntervalMs` further would likely buy
little, while widening `distanceIntervalM` may buy more.

**Confounds that remain:** one device, one person, 42 discharging hours after
against 29 before, spread over eleven days rather than two. The direction is
clear and the magnitude is not precise.

**Not measured:** the fidelity cost. Reconciliation needs stops on both sides
of the change to compare, and the drive pattern since has produced too few.

## Alternatives considered

**Key on accuracy tier rather than frequency** — `High` to `Balanced`, letting
Android power down GPS. **This is what I recommended first, and it was wrong.**
It came from four hours of a single afternoon in which 38 fixes and 186 fixes
cost the same 5%, which looked like proof that frequency did not matter. Thirty
hours across four days say the opposite, clearly and with driving controlled
for. Recorded here because the reversal is the useful part: the first
conclusion was confident, cheap to reach, and wrong, and the only thing that
corrected it was more data.

Accuracy tiering may still be worth adding later. It is a second lever, not the
first one.

**Key on charge level alone.** Simpler, and monotonic — charge only falls.
Rejected because it ignores the finding above: at a given charge, sampling rate
is what moves the number.

**Activity recognition instead of a speed threshold.** Google's
`ActivityRecognitionClient` knows "in vehicle" versus "still" far better than a
speed threshold can, and would make the motion input much stronger — only 6 of
108 fixes on one drive exceeded 5 m/s, so speed is a weak signal at low speeds.
Deferred: it needs either a native config plugin or `expo-sensors` plus our own
classifier, and the slice can be built and measured without it.

**Do nothing.** 17.9 hours is survivable. Rejected because the brief asks this
project to demonstrate the battery-versus-fidelity tradeoff explicitly, and
"we did not measure" is the one answer that cannot survive an interview.

## Consequences

**Thresholds will be wrong at first.** The 90-second dwell threshold in ADR-007
was a guess and is still a guess; these numbers come from one device, one model,
one person's driving. The honest claim is a measured baseline and a direction,
not a tuned system. The high-rate stationary bucket is **3 hours**, so the 4×
figure is provisional.

**Restarting the subscription has an unmeasured cost.** Hysteresis assumes a
restart is expensive enough to avoid but cheap enough to do occasionally.
Neither half is measured. If tier changes turn out to cost more than they save,
that shows up as drain *rising* after this slice ships — which is why the
before-and-after comparison matters more than the policy.

**Sampling less means the server derives from fewer points.** Trips get coarser
and stop boundaries fuzzier. Reconciliation is how that becomes visible rather
than silent.

**We would revisit this** if activity recognition became available to the
detector, if a second device showed a materially different baseline — one phone
is not a fleet — or if the reconciliation delta grew past the point where the
derived data stopped being trustworthy.
