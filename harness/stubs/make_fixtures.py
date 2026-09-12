#!/usr/bin/env python3
"""Write the tiny synthetic fixture inputs used by the offline selftest (stubs/fixture_inputs/{v221,v201}).

These are NOT the conformance testdata (that is testdata/, owned by the spec). They only carry the
markers the stub PAS server keys on (Claim.identifier value containing "deny" or "pend") and the input
names the real suites use.
"""

import json
from pathlib import Path

OUT = Path(__file__).resolve().parent / "fixture_inputs"
BASE = "http://example.org/nirmitee-synthetic/fhir"


def bundle(marker, items=(1,), related=None):
    claim = {
        "resourceType": "Claim", "id": f"claim-{marker}",
        "identifier": [{"system": "http://example.org/nirmitee-synthetic/claim", "value": marker}],
        "status": "active",
        "type": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/claim-type", "code": "professional"}]},
        "use": "preauthorization", "created": "2026-09-01T10:00:00Z",
        "patient": {"reference": "Patient/pat-synthetic-1"},
        "insurer": {"reference": "Organization/org-payer-1"},
        "provider": {"reference": "Organization/org-provider-1"},
        "priority": {"coding": [{"system": "http://terminology.hl7.org/CodeSystem/processpriority", "code": "normal"}]},
        "insurance": [{"sequence": 1, "focal": True, "coverage": {"reference": "Coverage/cov-1"}}],
        "item": [{"sequence": n, "productOrService": {"coding": [
            {"system": "http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets", "code": "G0154"}]}} for n in items],
    }
    if related:
        claim["related"] = [{"claim": {"reference": related}, "relationship": {"coding": [
            {"system": "http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship", "code": "prior"}]}}]
    return {
        "resourceType": "Bundle", "id": f"bundle-{marker}",
        "identifier": {"system": "http://example.org/nirmitee-synthetic/transaction", "value": f"tx-{marker}"},
        "type": "collection", "timestamp": "2026-09-01T10:00:00Z",
        "entry": [
            {"fullUrl": f"{BASE}/Claim/claim-{marker}", "resource": claim},
            {"fullUrl": f"{BASE}/Patient/pat-synthetic-1", "resource": {"resourceType": "Patient", "id": "pat-synthetic-1",
                                                                       "name": [{"family": "Synthetic", "given": ["Test"]}]}},
            {"fullUrl": f"{BASE}/Organization/org-payer-1", "resource": {"resourceType": "Organization", "id": "org-payer-1",
                                                                        "name": "Synthetic Payer"}},
            {"fullUrl": f"{BASE}/Organization/org-provider-1", "resource": {"resourceType": "Organization",
                                                                           "id": "org-provider-1", "name": "Synthetic Clinic"}},
        ],
    }


def subscription(content):
    return {
        "resourceType": "Subscription", "status": "requested", "end": "2030-12-31T00:00:00Z",
        "reason": "Synthetic PAS notifications",
        "criteria": "http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic",
        "_criteria": {"extension": [{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria",
                                     "valueString": "org-identifier=1234567893"}]},
        "channel": {"type": "rest-hook", "endpoint": "http://placeholder.invalid/notifications",
                    "payload": "application/fhir+json",
                    "_payload": {"extension": [{"url": "http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content",
                                                "valueCode": content}]}},
    }


def write(version, files, inputs):
    folder = OUT / version
    folder.mkdir(parents=True, exist_ok=True)
    for name, data in files.items():
        (folder / name).write_text(json.dumps(data, indent=2) + "\n")
    spec = {"suite_id": f"davinci_pas_server_suite_{version}",
            "inputs": [{"name": "server_endpoint", "value": "${FHIR_BASE}"},
                       {"name": "access_token", "value": "${NOTIFICATION_TOKEN}"}] + inputs}
    (folder / "inputs.json").write_text(json.dumps(spec, indent=2) + "\n")


def main():
    common = {
        "approval.json": bundle("approve-001"),
        "denial.json": bundle("deny-001"),
        "pended.json": bundle("pend-001"),
        "ms_submit.json": [bundle("approve-ms-1"), bundle("approve-ms-2", items=(1, 2))],
        "ms_inquire.json": [bundle("inquire-ms-1")],
    }
    common_inputs = [
        {"name": "subscription_resource", "file": "subscription.json"},
        {"name": "approval_pa_submit_request_payload", "file": "approval.json"},
        {"name": "denial_pa_submit_request_payload", "file": "denial.json"},
        {"name": "pended_pa_submit_request_payload", "file": "pended.json"},
        {"name": "must_support_pa_submit_request_payload", "file": "ms_submit.json"},
        {"name": "must_support_pa_inquire_request_payload", "file": "ms_inquire.json"},
    ]
    v221 = dict(common, **{
        "subscription.json": subscription("full-resource"),
        "cu_initial.json": bundle("update-initial", items=(1,)),
        "cu_add_item.json": bundle("update-add", items=(1, 2), related=f"{BASE}/Claim/claim-update-initial"),
        "cu_modify_cancel.json": bundle("update-modify", items=(1, 2), related=f"{BASE}/Claim/claim-update-add"),
        "cu_cancel_all.json": bundle("update-cancel", items=(), related=f"{BASE}/Claim/claim-update-modify"),
    })
    write("v221", v221, common_inputs + [
        {"name": "claim_update_initial_submit_payload", "file": "cu_initial.json"},
        {"name": "claim_update_add_item_submit_payload", "file": "cu_add_item.json"},
        {"name": "claim_update_modify_cancel_submit_payload", "file": "cu_modify_cancel.json"},
        {"name": "claim_update_cancel_all_submit_payload", "file": "cu_cancel_all.json"},
    ])
    v201 = dict(common, **{"subscription.json": subscription("id-only"), "pended_inquire.json": bundle("inquire-pend-001")})
    write("v201", v201, common_inputs + [{"name": "pended_pa_inquire_request_payload", "file": "pended_inquire.json"}])
    print(f"fixtures written under {OUT}")


if __name__ == "__main__":
    main()
