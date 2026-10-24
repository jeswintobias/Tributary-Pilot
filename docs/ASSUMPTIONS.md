# Assumptions

> Every number produced by this repository is a **modelled estimate from synthetic
> data**. Nothing here is a measurement of a real corridor, a real cost or a real
> capital saving. Where we state a figure, the generating assumption is written
> next to it so a reviewer can disagree with the assumption rather than the
> arithmetic.

## 1. Synthetic flow data

`agent/data/generate.py` produces hourly transfer volume for four corridors:
UAE→IN, UK→IN, US→IN and IN→UAE. No real transaction data, from any institution,
is used anywhere in this project.

### 1.1 Shape of the signal

| Component | Assumption | Rationale |
|---|---|---|
| Base level | Each corridor has a fixed hourly mean, with UAE→IN the largest | The Gulf is the largest single source of inward remittance to India |
| Salary window | Volume lifts from the 25th of the month to the 5th of the next | Gulf and UK payroll lands at month end; migrant workers remit on receipt |
| Festival spikes | Multiplicative bumps around Onam, Diwali and Eid, tapering over several days | Remittance spikes ahead of festivals, with a pre-festival ramp rather than a single-day step |
| Weekly pattern | Weekend behaviour differs by corridor; the Gulf working week differs from the UK and US | Fri/Sat weekend in the UAE versus Sat/Sun in the UK and US |
| Hour of day | Two daily peaks, shifted per corridor by its timezone offset from IST | Senders transact outside their own working hours |
| Noise | Multiplicative lognormal noise on the final level | Volume is positive and right-skewed; additive Gaussian noise would produce negative volumes |
| Return flow | IN→UAE is materially smaller than UAE→IN but correlated with it | Netting only helps if opposite flows exist at all; the asymmetry is what makes the netting benefit finite |

### 1.2 What the data deliberately does not model

Regime changes, corridor-level policy shocks, competitor pricing, a correlated
crisis across all corridors at once, and intraday autocorrelation beyond the shape
above. A forecaster that looks good on this data has not been shown to work on
real flows, and we do not claim it has.

## 2. Forecasting

- The target is hourly corridor demand. We fit a **quantile** model for P50 and P90,
  not a point forecast, because pool sizing needs the tail and not the mean.
- Model: LightGBM with quantile objective. Features are calendar and lag based.
- Train/test is a **time-ordered** split, never a random shuffle; a random split
  would leak future information through the lag features and flatter the result.
- Reported errors are on the held-out tail of the series only.

## 3. Pool-sizing comparison

This is the central quantitative claim of the pitch, so it is the one to be most
careful about.

### 3.1 What is compared

Two ways to cover the same forecast demand at the same shortfall risk:

- **Separate buffers.** Each provider sizes its own buffer independently against
  its own corridor demand at a chosen quantile.
- **One shared pool.** A single buffer covers the aggregate, sized at the same
  quantile against aggregate demand, after netting opposite flows within the
  window.

### 3.2 Assumptions behind it

| Assumption | Effect if wrong |
|---|---|
| Shortfall risk is held equal between the two designs — we compare buffer sizes at matched risk, not buffer sizes at matched cost | If risk is not matched, the comparison is meaningless |
| Corridor demands are **imperfectly correlated**; the diversification benefit comes from exactly that | Perfect correlation would reduce the benefit toward zero |
| Netting window is short enough that opposite flows genuinely overlap within it | A long window overstates netting; a window shorter than the flow pattern understates it |
| Pooled members can actually draw on the pool when needed, i.e. no member-level gating during stress | If gating exists under stress, the effective pooled buffer is smaller than modelled |
| Operational cost of running the pool is excluded | A real decision needs it; our figure is capital efficiency only |

### 3.3 How to read the output

The notebook reports the reduction in required buffer as a **percentage at a stated
quantile on this synthetic series**. It is an illustration of the diversification
and netting mechanism, not a projection of savings for any real institution. Any
slide or submission text that cites it must carry the same qualification.

## 4. Cost model

Phase 0 contains **no cost model and no fee figures.** The pitch mentions that
remittance into India is expensive; the supporting figure for that belongs in
submission text with a citation to a published source (World Bank Remittance
Prices Worldwide, or RBI), not in this repository. We will not invent a
corridor-level fee breakdown.

## 5. Capital-savings model

Likewise, we quote **no rupee or dollar capital saving.** The notebook's output is
a buffer-size ratio under the assumptions in §3. Converting that to a currency
amount requires a balance-sheet size and a cost of capital that we do not have and
will not assume.

## 6. Environment

Phase 0 is being developed on macOS on Apple silicon (arm64), while the DRUNIX
sample network documents **Linux** as its prerequisite. Any platform-specific
workaround is recorded in `network/README.md` with the exact versions that worked.
Where the network could not be brought up, the README status table says so rather
than implying local verification.

## 7. Currencies in the running example

Ravi, the 2,200 AED transfer, the ₹30,000 college fee and the Madurai payout are a
narrative device used consistently across the docs and the demo. There is no real
person, no real account and no real institution behind any of it.
