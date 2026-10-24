# Pilot — the AI liquidity agent

> Phase 0 delivers the forecasting half only: a synthetic data generator and a
> quantile forecasting notebook with a pool-sizing comparison. The optimiser, the
> policy-gated action submission and the audit log are Phase 1. See the
> [README status table](../README.md#status).

## The governance rule this agent is built around

**The agent only proposes. A policy chaincode decides.** Pilot holds no key that
can move money. In Phase 1 every action it wants to take — a top-up, an auction,
an intraday repo — is submitted as a proposal through the `policy` contract,
which enforces limits, approved members and maker-checker before anything
happens. An optional LLM layer explains decisions in plain English; it performs
no arithmetic and signs no transaction.

That split is the reason a mis-forecast is a cost problem and not a solvency
problem. It is threat T3.1 in [`docs/THREAT_MODEL.md`](../docs/THREAT_MODEL.md).

## Setup

```bash
cd agent
python3 -m venv .venv
./.venv/bin/pip install numpy pandas lightgbm scikit-learn matplotlib jupyter
```

On macOS LightGBM also needs OpenMP: `brew install libomp`.

Versions verified here: Python 3.12.0, numpy 2.5.4, pandas 3.1.0, lightgbm 4.7.0,
scikit-learn 1.9.1, matplotlib 3.11.2.

## Synthetic data

```bash
cd data
../.venv/bin/python generate.py --out flows.csv
```

Produces hourly flows for four corridors — UAE→IN, UK→IN, US→IN and IN→UAE —
with a salary window, festival ramps, corridor-specific weekends, a
timezone-shifted daily profile and multiplicative lognormal noise. Default range
is Jan 2024 to Oct 2026, which is 96,384 rows. Deterministic given `--seed`.

**No real transaction data, from any institution, is used anywhere in this
project.** Every generating assumption is documented at the top of
[`generate.py`](data/generate.py) and in
[`docs/ASSUMPTIONS.md` §1](../docs/ASSUMPTIONS.md).

Volumes are emitted as integer INR paise, converted from each corridor's own
minor unit at a fixed notional rate. The rate is a modelling convenience and is
never treated as a market rate. Keeping the column integral follows the same
no-floating-point-money rule the chaincode does.

## Forecasting notebook

```bash
cd notebooks
../.venv/bin/python -m jupyter lab forecast.ipynb
```

[`forecast.ipynb`](notebooks/forecast.ipynb) fits one LightGBM quantile model per
corridor for P50 and P90, on a **time-ordered** split — a random shuffle would
leak future values through the lag features and flatter every error metric in it.
It then compares separate per-provider buffers against one shared netted pool at
**matched shortfall risk**, and checks the realised shortfall frequency of both
designs to confirm the risk really was matched.

Read §6 of the notebook before quoting any number from it. The headline figure is
a modelled estimate on synthetic data under stated assumptions, not a projection
of savings for any real institution.
