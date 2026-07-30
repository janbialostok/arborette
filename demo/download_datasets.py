#!/usr/bin/env python3
"""
Download and prepare the demo datasets from Kaggle.

Usage:
    python3 demo/download_datasets.py              # default sample
    python3 demo/download_datasets.py --olist-rows 10000  # larger sample
    python3 demo/download_datasets.py --olist-full         # all rows (~113k)
"""

import argparse
import os
import sys

import kagglehub
import pandas as pd


def download_insurance(output_dir: str) -> str:
    print("Downloading Medical Cost Personal Datasets...")
    path = kagglehub.dataset_download("mirichoi0218/insurance")
    src = os.path.join(path, "insurance.csv")
    dst = os.path.join(output_dir, "insurance.csv")
    df = pd.read_csv(src)
    df.to_csv(dst, index=False)
    print(f"  {len(df)} rows -> {dst}")
    print(f"  Columns: {list(df.columns)}")
    print(f"  Charges: ${df['charges'].min():.2f} - ${df['charges'].max():.2f}")
    return dst


def download_olist(output_dir: str, nrows: int | None = None) -> str:
    print("Downloading Brazilian E-commerce...")
    path = kagglehub.dataset_download("olistbr/brazilian-ecommerce")

    orders = pd.read_csv(os.path.join(path, "olist_orders_dataset.csv"))
    items = pd.read_csv(os.path.join(path, "olist_order_items_dataset.csv"))
    products = pd.read_csv(os.path.join(path, "olist_products_dataset.csv"))
    customers = pd.read_csv(os.path.join(path, "olist_customers_dataset.csv"))
    category_trans = pd.read_csv(os.path.join(path, "product_category_name_translation.csv"))

    m = items.merge(orders, on="order_id", how="left")
    m = m.merge(customers, on="customer_id", how="left")
    m = m.merge(products, on="product_id", how="left")
    m = m.merge(category_trans, on="product_category_name", how="left")
    m["total_value"] = m["price"] + m["freight_value"]

    cols = [
        "order_id", "customer_unique_id", "customer_state", "customer_city",
        "product_category_name_english", "price", "freight_value", "total_value",
        "order_status", "order_purchase_timestamp",
        "order_estimated_delivery_date", "order_delivered_customer_date",
    ]
    flat = m[cols].dropna(subset=["total_value", "customer_state"]).copy()
    flat["product_category_name_english"] = flat["product_category_name_english"].fillna("unknown")

    if nrows is not None and nrows < len(flat):
        flat = flat.sample(n=nrows, random_state=42).reset_index(drop=True)

    dst = os.path.join(output_dir, "olist_orders.csv")
    flat.to_csv(dst, index=False)
    print(f"  {len(flat)} rows -> {dst}")
    print(f"  Columns: {list(flat.columns)}")
    print(f"  Total value: ${flat['total_value'].min():.2f} - ${flat['total_value'].max():.2f}")
    print(f"  States: {sorted(flat['customer_state'].unique())}")
    cats = sorted(flat["product_category_name_english"].unique())
    print(f"  Categories: {cats[:5]}... ({len(cats)} total)")
    return dst


def main():
    parser = argparse.ArgumentParser(description="Download Arborette demo datasets from Kaggle")
    parser.add_argument("--olist-rows", type=int, default=5000,
                        help="Number of Olist order rows to sample (default: 5000)")
    parser.add_argument("--olist-full", action="store_true",
                        help="Use all Olist rows (~113k, may be slow)")
    parser.add_argument("--output-dir", default=None,
                        help="Output directory (default: local-import/)")
    args = parser.parse_args()

    script_dir = os.path.dirname(os.path.abspath(__file__))
    output_dir = args.output_dir or os.path.join(script_dir, "..", "local-import")
    os.makedirs(output_dir, exist_ok=True)

    download_insurance(output_dir)
    print()
    download_olist(output_dir, nrows=None if args.olist_full else args.olist_rows)
    print()
    print("Done. Datasets ready in", output_dir)
    print("Run: bash demo/run_insurance_demo.sh")
    print("Run: bash demo/run_olist_demo.sh")


if __name__ == "__main__":
    main()
