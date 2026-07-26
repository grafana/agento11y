from __future__ import annotations

import hashlib
from dataclasses import asdict, dataclass
from datetime import date, timedelta


@dataclass(frozen=True)
class Product:
    sku: str
    name: str
    category: str
    region: str
    warehouse: str
    available: int
    reserved: int
    reorder_point: int
    unit_price_usd: int


PRODUCTS: list[Product] = [
    Product("ENV-100", "Air Quality Sensor", "environment", "US", "Northeast", 42, 8, 20, 149),
    Product("ENV-220", "Smart Thermostat", "environment", "US", "West", 17, 4, 15, 229),
    Product("KIT-310", "Countertop Mixer", "kitchen", "US", "South", 9, 2, 12, 299),
    Product("KIT-440", "Compact Air Fryer", "kitchen", "US", "Midwest", 64, 11, 25, 129),
    Product("HOM-510", "Robot Vacuum", "home", "US", "Northeast", 6, 3, 10, 399),
    Product("HOM-620", "Portable Air Purifier", "home", "US", "West", 27, 5, 18, 199),
]


ORDER_STATUS: dict[str, dict[str, str]] = {
    "DEMO-1007": {
        "account_id": "acct-demo-1042",
        "status": "delayed",
        "eta": (date.today() + timedelta(days=4)).isoformat(),
        "summary": "Carrier exception on 8 ENV-100 units. Replacement stock is available.",
    },
    "DEMO-1012": {
        "account_id": "acct-demo-1088",
        "status": "in_transit",
        "eta": (date.today() + timedelta(days=2)).isoformat(),
        "summary": "KIT-440 shipment departed the Midwest warehouse.",
    },
    "DEMO-1020": {
        "account_id": "acct-demo-1103",
        "status": "delivered",
        "eta": date.today().isoformat(),
        "summary": "HOM-620 order was delivered and signed for.",
    },
}


SUPPORT_CASES: dict[str, list[dict[str, str]]] = {
    "ENV-100": [
        {
            "case_id": "CASE-8821",
            "severity": "medium",
            "summary": "Packaging damage reported on two units from DEMO-1007.",
        }
    ],
    "HOM-510": [
        {
            "case_id": "CASE-8814",
            "severity": "high",
            "summary": "Low inventory risk for existing bundle commitments.",
        }
    ],
}


def search_products(query: str, region: str = "US") -> list[dict[str, object]]:
    normalized = query.casefold().strip()
    region_normalized = region.casefold().strip()
    matches: list[Product] = []
    for product in PRODUCTS:
        haystack = f"{product.sku} {product.name} {product.category} {product.warehouse}".casefold()
        if region_normalized and product.region.casefold() != region_normalized:
            continue
        if normalized in haystack or any(token in haystack for token in normalized.split()):
            matches.append(product)
    return [_product_to_dict(item) for item in matches[:8]]


def _product_to_dict(product: Product) -> dict[str, object]:
    result: dict[str, object] = asdict(product)
    net_available = product.available - product.reserved
    result["net_available"] = net_available
    result["low_stock"] = net_available <= product.reorder_point
    return result


def get_order(order_id: str) -> dict[str, str] | None:
    return ORDER_STATUS.get(order_id.strip().upper())


def get_support_cases(sku: str) -> list[dict[str, str]]:
    return SUPPORT_CASES.get(sku.strip().upper(), [])


def make_return(
    account_id: str,
    sku: str,
    quantity: int,
    reason: str,
) -> dict[str, object]:
    safe_account_id = account_id.strip() or "acct-unknown"
    safe_sku = sku.strip().upper()
    safe_quantity = max(1, int(quantity))
    return_key = f"{safe_account_id}:{safe_sku}:{safe_quantity}:{reason}".encode()
    suffix = int(hashlib.sha256(return_key).hexdigest()[:8], 16) % 100000
    return {
        "return_id": f"RETURN-{suffix:05d}",
        "account_id": safe_account_id,
        "sku": safe_sku,
        "quantity": safe_quantity,
        "reason": reason.strip() or "Not specified",
        "status": "created",
        "next_step": "Send a prepaid label and reserve replacement stock if available.",
    }
