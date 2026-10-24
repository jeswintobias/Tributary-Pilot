"""Synthetic hourly corridor flow generator for Tributary Pilot.

No real transaction data, from any institution, is used anywhere in this
project. Every number this script produces is manufactured from the assumptions
documented in docs/ASSUMPTIONS.md section 1, and exists only to exercise the
forecaster and the pool-sizing comparison.

Usage:
    python generate.py --out flows.csv
    python generate.py --start 2024-01-01 --end 2026-10-01 --seed 7

Output columns:
    ts            hourly timestamp (UTC, inclusive of start, exclusive of end)
    corridor      one of UAE_IN, UK_IN, US_IN, IN_UAE
    volume_paise  flow value in INR paise, as an integer
    txn_count     number of transfers in the hour, as an integer

Why paise for every corridor: the pool-sizing comparison needs one unit. Foreign
legs are converted at a fixed notional rate declared in NOTIONAL_RATE_PAISE
below. That is a modelling convenience, not a market rate, and nothing in this
repository treats it as one. Keeping the column integral is deliberate: the same
no-floating-point-money rule the chaincode follows.
"""

from __future__ import annotations

import argparse
import datetime as dt
import sys

import numpy as np
import pandas as pd

# ---------------------------------------------------------------------------
# Assumptions. Every constant here is a modelling choice, not a measurement.
# ---------------------------------------------------------------------------

CORRIDORS = ("UAE_IN", "UK_IN", "US_IN", "IN_UAE")

# Mean transfers per hour at baseline. UAE_IN is the largest because the Gulf is
# the largest single source of inward remittance to India. IN_UAE is small but
# non-zero: netting only helps if an opposite flow exists at all, and the
# asymmetry is what makes the netting benefit finite rather than total.
BASE_TXN_PER_HOUR = {
    "UAE_IN": 420.0,
    "UK_IN": 210.0,
    "US_IN": 300.0,
    "IN_UAE": 70.0,
}

# Mean transfer size in the sending currency's minor units.
MEAN_TICKET_MINOR = {
    "UAE_IN": 180_000,      # 1,800 AED in fils
    "UK_IN": 65_000,        # £650 in pence
    "US_IN": 90_000,        # $900 in cents
    "IN_UAE": 12_000_000,   # ₹120,000 in paise
}

# Fixed notional conversion to INR paise per unit of the sending minor unit.
# A modelling constant, deliberately crude, never a market rate.
NOTIONAL_RATE_PAISE = {
    "UAE_IN": 2_270,   # 1 fil  -> 22.70 paise  (~22.7 INR per AED)
    "UK_IN": 10_600,   # 1 pence -> 106 paise   (~106 INR per GBP)
    "US_IN": 8_300,    # 1 cent  -> 83 paise    (~83 INR per USD)
    "IN_UAE": 100,     # already paise
}
RATE_SCALE = 100  # NOTIONAL_RATE_PAISE is expressed in hundredths of a paise

# Hours to shift the daily shape by, per corridor, approximating the sender's
# local day relative to IST.
TZ_SHIFT_HOURS = {"UAE_IN": -1, "UK_IN": -5, "US_IN": -10, "IN_UAE": 0}

# Weekend days as Python weekday numbers (Monday=0). The Gulf working week
# differs from the UK and US one.
WEEKEND = {
    "UAE_IN": (4, 5),   # Fri, Sat
    "UK_IN": (5, 6),    # Sat, Sun
    "US_IN": (5, 6),
    "IN_UAE": (5, 6),
}
WEEKEND_MULTIPLIER = 0.72

# Salary window: payroll lands at month end and migrant workers remit on receipt.
SALARY_WINDOW_FROM_DAY = 25   # inclusive, to month end
SALARY_WINDOW_TO_DAY = 5      # inclusive, from month start
SALARY_MULTIPLIER = 1.55

# Festival dates, approximate, for the years this generator covers. Remittance
# lifts ahead of a festival rather than on the day, so each is modelled as a ramp
# over the preceding days.
FESTIVALS = {
    "Onam": ["2024-09-15", "2025-09-05", "2026-08-26"],
    "Diwali": ["2024-11-01", "2025-10-20", "2026-11-08"],
    "Eid al-Fitr": ["2024-04-10", "2025-03-30", "2026-03-20"],
    "Eid al-Adha": ["2024-06-16", "2025-06-06", "2026-05-27"],
}
FESTIVAL_RAMP_DAYS = 10     # how long before the date the lift begins
FESTIVAL_PEAK_MULTIPLIER = {
    # Onam matters most for the Gulf-to-Kerala flow; Eid for the Gulf corridors;
    # Diwali broadly.
    "Onam": {"UAE_IN": 1.80, "UK_IN": 1.25, "US_IN": 1.20, "IN_UAE": 1.10},
    "Diwali": {"UAE_IN": 1.55, "UK_IN": 1.50, "US_IN": 1.50, "IN_UAE": 1.15},
    "Eid al-Fitr": {"UAE_IN": 1.65, "UK_IN": 1.20, "US_IN": 1.10, "IN_UAE": 1.30},
    "Eid al-Adha": {"UAE_IN": 1.45, "UK_IN": 1.15, "US_IN": 1.08, "IN_UAE": 1.25},
}

# Multiplicative lognormal noise. Volume is positive and right-skewed, so
# additive Gaussian noise would eventually produce negative volumes.
NOISE_SIGMA = 0.22

# IN_UAE is correlated with UAE_IN — the same corridor relationship drives both
# directions — but only partially.
RETURN_FLOW_CORRELATION = 0.45


def _daily_shape(hour_ist: np.ndarray, shift: int) -> np.ndarray:
    """Two daily peaks, shifted into the sender's local day.

    Senders transact outside their own working hours, which gives a morning and
    an evening peak rather than a single business-hours hump.
    """
    h = (hour_ist + shift) % 24
    morning = np.exp(-0.5 * ((h - 9) / 2.2) ** 2)
    evening = np.exp(-0.5 * ((h - 20) / 2.6) ** 2)
    return 0.45 + 0.85 * morning + 1.15 * evening


def _salary_multiplier(day: np.ndarray) -> np.ndarray:
    in_window = (day >= SALARY_WINDOW_FROM_DAY) | (day <= SALARY_WINDOW_TO_DAY)
    return np.where(in_window, SALARY_MULTIPLIER, 1.0)


def _festival_multiplier(index: pd.DatetimeIndex, corridor: str) -> np.ndarray:
    """Ramp up to each festival date, then fall away sharply after it."""
    mult = np.ones(len(index), dtype=float)
    dates = index.normalize()
    for name, days in FESTIVALS.items():
        peak = FESTIVAL_PEAK_MULTIPLIER[name][corridor]
        for day in days:
            festival_day = pd.Timestamp(day)
            days_before = (festival_day - dates).days
            # Linear ramp over FESTIVAL_RAMP_DAYS, nothing after the day itself.
            in_ramp = (days_before >= 0) & (days_before <= FESTIVAL_RAMP_DAYS)
            ramp = np.where(
                in_ramp,
                1.0 + (peak - 1.0) * (1.0 - days_before / FESTIVAL_RAMP_DAYS),
                1.0,
            )
            mult = np.maximum(mult, ramp)
    return mult


def generate(start: str, end: str, seed: int) -> pd.DataFrame:
    """Generate hourly flows for every corridor between start and end."""
    index = pd.date_range(start=start, end=end, freq="h", inclusive="left")
    if len(index) == 0:
        raise ValueError(f"empty date range: {start} to {end}")

    rng = np.random.default_rng(seed)
    hour = index.hour.to_numpy()
    day = index.day.to_numpy()
    weekday = index.day_of_week.to_numpy()

    # Draw the UAE_IN noise first so the return flow can be correlated with it.
    noise = {c: rng.normal(0.0, NOISE_SIGMA, len(index)) for c in CORRIDORS}
    noise["IN_UAE"] = (
        RETURN_FLOW_CORRELATION * noise["UAE_IN"]
        + np.sqrt(1.0 - RETURN_FLOW_CORRELATION**2) * noise["IN_UAE"]
    )

    frames = []
    for corridor in CORRIDORS:
        level = (
            BASE_TXN_PER_HOUR[corridor]
            * _daily_shape(hour, TZ_SHIFT_HOURS[corridor])
            * _salary_multiplier(day)
            * _festival_multiplier(index, corridor)
            * np.where(np.isin(weekday, WEEKEND[corridor]), WEEKEND_MULTIPLIER, 1.0)
            * np.exp(noise[corridor] - 0.5 * NOISE_SIGMA**2)  # mean-preserving
        )

        txn_count = rng.poisson(np.clip(level, 0.0, None))

        # Ticket size varies per hour too, but much less than volume does.
        ticket_minor = MEAN_TICKET_MINOR[corridor] * np.exp(
            rng.normal(0.0, 0.10, len(index)) - 0.5 * 0.10**2
        )
        volume_minor = txn_count * ticket_minor

        # Convert to INR paise and round to an exact integer. Money stays
        # integral here for the same reason it does in the chaincode.
        volume_paise = np.rint(
            volume_minor * NOTIONAL_RATE_PAISE[corridor] / RATE_SCALE
        ).astype("int64")

        frames.append(
            pd.DataFrame(
                {
                    "ts": index,
                    "corridor": corridor,
                    "volume_paise": volume_paise,
                    "txn_count": txn_count.astype("int64"),
                }
            )
        )

    out = pd.concat(frames, ignore_index=True)
    return out.sort_values(["ts", "corridor"], ignore_index=True)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    parser.add_argument("--start", default="2024-01-01")
    parser.add_argument("--end", default="2026-10-01")
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--out", default="flows.csv")
    args = parser.parse_args(argv)

    df = generate(args.start, args.end, args.seed)
    df.to_csv(args.out, index=False)

    print(f"wrote {len(df):,} rows to {args.out}")
    print(f"range: {df.ts.min()} .. {df.ts.max()}  seed={args.seed}")
    summary = (
        df.groupby("corridor")
        .agg(
            hours=("volume_paise", "size"),
            mean_inr_per_hour=("volume_paise", lambda s: s.mean() / 100),
            p90_inr_per_hour=("volume_paise", lambda s: s.quantile(0.90) / 100),
            total_txns=("txn_count", "sum"),
        )
        .round(0)
    )
    print(summary.to_string())
    print("\nSynthetic data. See docs/ASSUMPTIONS.md section 1.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
