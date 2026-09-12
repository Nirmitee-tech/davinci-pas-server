#!/usr/bin/env python3
"""Executable reference of the Nirmitee PAS server contract (spec/SPEC.md).

This is NOT a server. It is a pure in-memory model of what the Go and .NET servers must return, used to:
  1. generate the canonical example payloads in spec/examples/<version>/ (responses, notifications,
     CapabilityStatement, Subscription, OperationOutcome) from the synthetic testdata in testdata/;
  2. prove offline, with spec/tools/pas_checks.py, that the testdata plus the rules in
     spec/adjudication-rules.json satisfy the non-validator logic of the Inferno PAS server tests.

It replays the suite in Inferno group order: Subscription Setup, approval, denial, pended (+ finalize and
notification, + v2.0.1 inquiry), Claim Updates (v2.2.1), must support $submit and $inquire, error handling.

Usage: python3 spec/reference/pas_reference.py [--version v221|v201|all] [--no-write] [--kit-dir DIR]
The kit directory (davinci-pas-test-kit clone, used only for the kit's error test Bundle) comes from --kit-dir or
the PAS_TEST_KIT_DIR environment variable; without it an equivalent empty collection Bundle is used.
Exit code 0 when every offline check passes. Python 3 stdlib only. Synthetic data only.
"""
import argparse
import copy
import datetime as dt
import hashlib
import json
import os
import sys
import uuid

HERE = os.path.dirname(os.path.abspath(__file__))
SPEC_DIR = os.path.normpath(os.path.join(HERE, '..'))
PAS_DIR = os.path.normpath(os.path.join(SPEC_DIR, '..'))
TESTDATA = os.path.join(PAS_DIR, 'testdata')
sys.path.insert(0, os.path.join(SPEC_DIR, 'tools'))
import pas_checks as checks  # noqa: E402

PAS = 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/'
X12 = 'https://codesystem.x12.org/005010/'
PASTEMP = 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes'
LOINC = 'http://loinc.org'
NPI_SYS = 'http://hl7.org/fhir/sid/us-npi'
DAR_URL = 'http://hl7.org/fhir/StructureDefinition/data-absent-reason'
TOPIC = 'http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic'
BACKPORT = 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/'
SID_PAYER = 'https://pas.nirmitee.example/fhir/sid/'
ADJ_CATEGORY = {'coding': [{'system': 'http://terminology.hl7.org/CodeSystem/adjudication', 'code': 'submitted'}]}
FIXED_NOW = dt.datetime(2026, 9, 12, 10, 0, 0, tzinfo=dt.timezone.utc)


def uuid4like(seed):
    h = hashlib.sha1(seed.encode()).hexdigest()
    return f'{h[0:8]}-{h[8:12]}-4{h[13:16]}-a{h[17:20]}-{h[20:32]}'


def ext_value(resource, url, key):
    for e in (resource or {}).get('extension', []) or []:
        if e.get('url') == url:
            return e.get(key)
    return None


def first_code(cc):
    return ((cc or {}).get('coding') or [{}])[0].get('code')


def iter_references(value, skip_claimresponse_request=True):
    if isinstance(value, list):
        for v in value:
            yield from iter_references(v, skip_claimresponse_request)
    elif isinstance(value, dict):
        if isinstance(value.get('reference'), str):
            yield value['reference']
            return
        for k, v in value.items():
            if k == 'request' and value.get('resourceType') == 'ClaimResponse' and skip_claimresponse_request:
                continue
            if isinstance(v, (dict, list)):
                yield from iter_references(v, skip_claimresponse_request)


def operation_outcome(message, code='invalid', expression=None):
    issue = {'severity': 'error', 'code': code, 'details': {'text': message}, 'diagnostics': message}
    if expression:
        issue['expression'] = [expression]
    return {'resourceType': 'OperationOutcome', 'issue': [issue]}


class PasModel:
    def __init__(self, version, fhir_base, rules, now=FIXED_NOW, live=False):
        self.version = version
        self.v221 = version == 'v221'
        self.base = fhir_base.rstrip('/')
        self.rules = rules
        self.live = live
        self._fixed_now = now
        self.counter = 0
        self.records = {}
        self.subscriptions = {}
        self.outbox = []

    # ------------------------------------------------------------------ ids and time
    @property
    def now(self):
        return dt.datetime.now(dt.timezone.utc) if self.live else self._fixed_now

    def new_id(self, kind):
        self.counter += 1
        if self.live:
            return str(uuid.uuid4())
        return uuid4like(f'{self.version}|{kind}|{self.counter}')

    def claim_response_by_id(self, cr_id):
        for record in self.records.values():
            response = record.get('last_response') or {}
            for e in response.get('entry', []):
                r = e.get('resource') or {}
                if r.get('resourceType') == 'ClaimResponse' and r.get('id') == cr_id:
                    return r
        return None

    def instant(self):
        return self.now.strftime('%Y-%m-%dT%H:%M:%SZ')

    def today(self, plus_days=0):
        return (self.now + dt.timedelta(days=plus_days)).strftime('%Y-%m-%d')

    # ------------------------------------------------------------------ CapabilityStatement
    def metadata(self):
        label = '2.2.1' if self.v221 else '2.0.1'
        return {
            'resourceType': 'CapabilityStatement',
            'id': f'nirmitee-pas-server-{self.version}',
            'url': f'{self.base}/metadata',
            'version': '0.1.0',
            'name': 'NirmiteePASServer',
            'title': f'Nirmitee PAS Server (Da Vinci PAS {label})',
            'status': 'active',
            'experimental': True,
            'date': '2026-09-12',
            'publisher': 'Nirmitee',
            'description': f'Da Vinci Prior Authorization Support {label} payer server with synthetic adjudication rules.',
            'kind': 'instance',
            'software': {'name': 'Nirmitee PAS Server', 'version': '0.1.0'},
            'implementation': {'description': f'Nirmitee PAS Server {label}', 'url': self.base},
            'fhirVersion': '4.0.1',
            'format': ['application/fhir+json', 'json'],
            'rest': [{
                'mode': 'server',
                'resource': [
                    {'type': 'Claim',
                     'operation': [
                         {'name': 'submit', 'definition': 'http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-submit'},
                         {'name': 'inquire', 'definition': 'http://hl7.org/fhir/us/davinci-pas/OperationDefinition/Claim-inquiry'}]},
                    {'type': 'ClaimResponse', 'interaction': [{'code': 'read'}]},
                    {'type': 'Subscription',
                     'extension': [{'url': BACKPORT + 'capabilitystatement-subscriptiontopic-canonical', 'valueCanonical': TOPIC}],
                     'interaction': [{'code': 'create'}, {'code': 'read'}, {'code': 'update'}, {'code': 'delete'}]},
                ],
            }],
        }

    # ------------------------------------------------------------------ request validation (400)
    def validate(self, body, operation):
        if not isinstance(body, dict) or body.get('resourceType') != 'Bundle':
            return operation_outcome('The request body must be a PAS Request Bundle.', 'structure', 'Bundle')
        entries = body.get('entry') or []
        if body.get('type') != 'collection':
            return operation_outcome('Bundle.type must be collection.', 'invalid', 'Bundle.type')
        if not entries:
            return operation_outcome('The PAS Bundle has no entries; the first entry must be a Claim.', 'required', 'Bundle.entry')
        claim = entries[0].get('resource') or {}
        if operation == 'inquire':
            claim = next((e['resource'] for e in entries if (e.get('resource') or {}).get('resourceType') == 'Claim'), {})
        if claim.get('resourceType') != 'Claim':
            return operation_outcome('The first Bundle entry must be a Claim.', 'invalid', 'Bundle.entry[0].resource')
        urls = {e.get('fullUrl') for e in entries}
        for field in ('patient', 'insurer', 'provider'):
            ref = (claim.get(field) or {}).get('reference')
            if not ref or (ref not in urls and not any(u and u.endswith('/' + ref) for u in urls)):
                return operation_outcome(f'Claim.{field} must reference a resource included in the Bundle.', 'invalid', f'Claim.{field}')
        if operation == 'submit' and not claim.get('item'):
            return operation_outcome('Claim.item is required for $submit.', 'required', 'Claim.item')
        return None

    # ------------------------------------------------------------------ adjudication
    def claim_cancel_code(self, claim):
        cc = ext_value(claim, PAS + 'extension-certificationType', 'valueCodeableConcept')
        return first_code(cc)

    def item_rule(self, claim, item):
        for rule in self.rules['itemRules']:
            w = rule['when']
            if w.get('itemModifierExtensionInfoCancelled'):
                if any(m.get('url') == PAS + 'modifierextension-infoCancelled' and m.get('valueBoolean') is True for m in item.get('modifierExtension', []) or []):
                    return rule
                continue
            if 'claimCertificationTypeCode' in w:
                if self.claim_cancel_code(claim) == w['claimCertificationTypeCode']:
                    return rule
                continue
            if 'productOrServiceCodes' in w:
                codes = [c.get('code') for c in (item.get('productOrService') or {}).get('coding', [])]
                if any(c in w['productOrServiceCodes'] for c in codes):
                    return rule
                continue
            if not w:
                return rule
        raise RuntimeError('no default rule')

    @staticmethod
    def claim_level(decisions):
        live = [d for d in decisions if d != 'cancelled']
        if not live:
            return 'cancelled'
        if 'pended' in live:
            return 'pended'
        if all(d == 'denied' for d in live):
            return 'denied'
        certified = [d for d in live if d in ('approved', 'modified', 'partial')]
        if 'denied' in live and certified:
            return 'partial'
        if 'modified' in live:
            return 'modified'
        return 'approved'

    @staticmethod
    def claim_key(claim):
        ident = (claim.get('identifier') or [{}])[0]
        return f"{ident.get('system')}|{ident.get('value')}"

    # ------------------------------------------------------------------ operations
    def submit(self, body):
        oo = self.validate(body, 'submit')
        if oo:
            return 400, oo
        claim = body['entry'][0]['resource']
        results = []
        for idx, item in enumerate(claim.get('item', [])):
            rule = self.item_rule(claim, item)
            results.append({'index': idx, 'sequence': item.get('sequence'), 'rule': rule, 'decision': rule['decision'], 'finalized': False})
        record = {'claim': claim, 'fullUrl': body['entry'][0].get('fullUrl'), 'bundle': body, 'results': results}
        self.records[self.claim_key(claim)] = record
        response = self.response_bundle('submit', claim, record, [body])
        record['last_response'] = response
        return 200, response

    def inquire(self, body):
        oo = self.validate(body, 'inquire')
        if oo:
            return 400, oo
        claim = next(e['resource'] for e in body['entry'] if e['resource']['resourceType'] == 'Claim')
        stored = self.records.get(self.claim_key(claim))
        results = []
        for idx, item in enumerate(claim.get('item', [])):
            found = None
            if stored:
                found = next((r for r in stored['results'] if r['sequence'] == item.get('sequence')), None)
            if found:
                stored_item = stored['claim']['item'][found['index']]
                results.append(dict(found, index=idx, stored_item=stored_item, stored_url=stored['fullUrl']))
            else:
                rule = self.item_rule(claim, item)
                results.append({'index': idx, 'sequence': item.get('sequence'), 'rule': rule, 'decision': rule['decision'], 'finalized': False})
        record = {'claim': claim, 'fullUrl': stored['fullUrl'] if stored else None, 'results': results, 'stored': stored}
        sources = [body] + ([stored['bundle']] if stored else [])
        bundle = self.response_bundle('inquire', claim, record, sources)
        if self.v221:
            return 200, {'resourceType': 'Parameters', 'parameter': [{'name': 'return', 'resource': bundle}]}
        return 200, bundle

    def create_subscription(self, body, header_token=None):
        if not isinstance(body, dict) or body.get('resourceType') != 'Subscription':
            return 400, operation_outcome('Body must be a Subscription.', 'structure')
        channel = body.get('channel') or {}
        content = next((x.get('valueCode') for x in ((channel.get('_payload') or {}).get('extension') or [])
                        if x.get('url') == BACKPORT + 'backport-payload-content'), None)
        errors = []
        if body.get('criteria') != TOPIC:
            errors.append('Subscription.criteria must be the PAS SubscriptionTopic canonical')
        if channel.get('type') != 'rest-hook':
            errors.append('Subscription.channel.type must be rest-hook')
        if not channel.get('endpoint'):
            errors.append('Subscription.channel.endpoint is required')
        expected = 'full-resource' if self.v221 else 'id-only'
        if content not in ('full-resource', 'id-only'):
            errors.append('backport-payload-content must be full-resource or id-only')
        if errors:
            return 422, operation_outcome('; '.join(errors), 'business-rule')
        sub = copy.deepcopy(body)
        sub['id'] = self.new_id('subscription')
        sub['status'] = 'requested'
        self.subscriptions[sub['id']] = {'resource': sub, 'events': 0, 'content': content or expected}
        self.outbox.append({'kind': 'handshake', 'subscription_id': sub['id'], 'headers': self.channel_headers(sub),
                            'endpoint': channel.get('endpoint'), 'body': self.handshake(sub)})
        return 201, sub

    def channel_headers(self, sub):
        headers = {'Content-Type': (sub.get('channel') or {}).get('payload') or 'application/fhir+json'}
        for h in (sub.get('channel') or {}).get('header', []) or []:
            name, _, value = h.partition(': ')
            headers[name] = value
        return headers

    def status_parameters(self, sub, kind, status, focus=None):
        sid = sub['id']
        params = [
            {'name': 'subscription', 'valueReference': {'reference': f'{self.base}/Subscription/{sid}'}},
            {'name': 'topic', 'valueCanonical': TOPIC},
            {'name': 'status', 'valueCode': status},
            {'name': 'type', 'valueCode': kind},
            {'name': 'events-since-subscription-start', 'valueString': str(self.subscriptions[sid]['events'])},
        ]
        if focus:
            params.append({'name': 'notification-event', 'part': [
                {'name': 'event-number', 'valueString': str(self.subscriptions[sid]['events'])},
                {'name': 'timestamp', 'valueInstant': self.instant()},
                {'name': 'focus', 'valueReference': {'reference': focus}}]})
        return {'resourceType': 'Parameters', 'id': self.new_id('status'), 'parameter': params}

    def status_entry(self, sub, parameters):
        return {'fullUrl': f"urn:uuid:{parameters['id']}", 'resource': parameters,
                'request': {'method': 'GET', 'url': f"{self.base}/Subscription/{sub['id']}/$status"},
                'response': {'status': '200'}}

    def handshake(self, sub):
        params = self.status_parameters(sub, 'handshake', 'requested')
        return {'resourceType': 'Bundle', 'id': self.new_id('handshake'), 'type': 'history', 'timestamp': self.instant(),
                'entry': [self.status_entry(sub, params)]}

    # Extensions the IG marks must support outside their declared context. The kit suppresses the resulting validator
    # errors for $submit and $inquire responses, but not when the ClaimResponse is nested in a full-resource
    # notification Bundle, so the notification copy omits them (SPEC.md 7.4).
    NOTIFICATION_OMIT = {
        'root': [PAS + 'extension-itemAuthorizedProvider'],
        'item': [PAS + 'extension-communicatedDiagnosis'],
        'addItem': [PAS + 'extension-productOrServiceCodeEnd', PAS + 'extension-revenueUnitRateLimit',
                    PAS + 'extension-serviceItemRequestType', PAS + 'extension-certificationType'],
    }

    def notification_copy(self, bundle):
        out = copy.deepcopy(bundle)
        for e in out.get('entry', []):
            cr = e.get('resource') or {}
            if cr.get('resourceType') != 'ClaimResponse':
                continue
            cr['extension'] = [x for x in cr.get('extension', []) if x.get('url') not in self.NOTIFICATION_OMIT['root']]
            if not cr['extension']:
                del cr['extension']
            for key in ('item', 'addItem'):
                for it in cr.get(key, []) or []:
                    it['extension'] = [x for x in it.get('extension', []) if x.get('url') not in self.NOTIFICATION_OMIT[key]]
        return out

    def finalize_pended(self):
        finalized = []
        for record in self.records.values():
            changed = False
            for r in record['results']:
                if r['decision'] == 'pended' and r['rule'].get('autoFinalize'):
                    r['decision'] = r['rule']['finalDecision']
                    r['finalized'] = True
                    changed = True
            if not changed:
                continue
            response = self.response_bundle('submit', record['claim'], record, [record['bundle']])
            record['last_response'] = response
            finalized.append(response)
            for sid, s in self.subscriptions.items():
                s['events'] += 1
                sub = s['resource']
                if s['content'] == 'full-resource':
                    focus = f"urn:uuid:{response['id']}"
                    params = self.status_parameters(sub, 'event-notification', 'active', focus)
                    entries = [self.status_entry(sub, params),
                               {'fullUrl': focus, 'resource': self.notification_copy(response),
                                'request': {'method': 'POST', 'url': f'{self.base}/Bundle'},
                                'response': {'status': '201'}}]
                else:
                    cr = response['entry'][0]['resource']
                    focus = f"{self.base}/ClaimResponse/{cr['id']}"
                    params = self.status_parameters(sub, 'event-notification', 'active', focus)
                    entries = [self.status_entry(sub, params),
                               {'fullUrl': focus, 'request': {'method': 'PUT', 'url': f"ClaimResponse/{cr['id']}"},
                                'response': {'status': '200'}}]
                note = {'resourceType': 'Bundle', 'id': self.new_id('event'), 'type': 'history', 'timestamp': self.instant(), 'entry': entries}
                self.outbox.append({'kind': 'event-notification', 'subscription_id': sid, 'headers': self.channel_headers(sub),
                                    'endpoint': sub['channel']['endpoint'], 'body': note})
        return finalized

    # ------------------------------------------------------------------ response construction
    def response_bundle(self, operation, claim, record, sources):
        v221 = self.v221
        entries_by_url = {}
        for b in sources:
            for e in b.get('entry', []):
                entries_by_url.setdefault(e.get('fullUrl'), e)
        claim_url = record.get('fullUrl') if operation == 'inquire' else sources[0]['entry'][0].get('fullUrl')
        ident_value = (claim.get('identifier') or [{}])[0].get('value', 'UNKNOWN')
        results = record['results']
        decisions = [r['decision'] for r in results]
        claim_decision = self.claim_level(decisions)
        D = self.rules['decisions']

        cr_id = self.new_id('claimresponse')
        cr_url = f'urn:uuid:{cr_id}'
        generated = {}

        def resolve(ref, context_url=None):
            if ref in entries_by_url or ref in generated:
                return ref
            if context_url and '/' in ref and not ref.startswith('urn:'):
                base = checks.extract_base_url(context_url)
                cand = f'{base}/{ref}' if base else ref
                if cand in entries_by_url:
                    return cand
            return ref

        care_team = {ct.get('sequence'): ct for ct in claim.get('careTeam', []) or []}
        overall = next((ct for ct in care_team.values() if ext_value(ct, PAS + 'extension-careTeamClaimScope', 'valueBoolean') is True), None)
        claim_provider = resolve((claim.get('provider') or {}).get('reference', ''), claim_url)

        def role_practitioner(role_url):
            role = (entries_by_url.get(role_url) or {}).get('resource') or {}
            return resolve((role.get('practitioner') or {}).get('reference', ''), role_url) if role.get('resourceType') == 'PractitionerRole' else None

        def role_location(role_url):
            role = (entries_by_url.get(role_url) or {}).get('resource') or {}
            locs = role.get('location') or []
            return resolve(locs[0].get('reference'), role_url) if locs else None

        supporting = {s.get('sequence'): s for s in claim.get('supportingInfo', []) or []}
        admission = next((s for s in supporting.values() if first_code(s.get('category')) == 'admissionDates'), None)
        discharge = next((s for s in supporting.values() if first_code(s.get('category')) == 'dischargeDates'), None)
        diagnoses = {d.get('sequence'): d for d in claim.get('diagnosis', []) or []}
        replaced = self.rules.get('replacedAuthorizations', {})
        pre_auth_period = {'start': self.today(), 'end': self.today(self.rules.get('preAuthPeriodDays', 90))}

        def adjudication(decision, number=None, reason=None):
            d = D[decision]
            sub = []
            if number:
                sub.append({'url': 'number', 'valueString': number})
            sub.append({'url': PAS + 'extension-reviewActionCode',
                        'valueCodeableConcept': {'coding': [{'system': self.rules['reviewActionCodeSystem'], 'code': d['code'], 'display': d['display']}]}})
            if reason:
                sub.append({'url': 'reasonCode', 'valueCodeableConcept': {'coding': [reason]}})
            return {'extension': [{'url': PAS + 'extension-reviewAction', 'extension': sub}], 'category': copy.deepcopy(ADJ_CATEGORY)}

        def auth_number(seq):
            return self.rules['authorizationNumberTemplate'].format(claimIdentifierValue=ident_value, itemSequence=seq)

        items, add_items, errors = [], [], []
        comm_request_needed, task_needed = None, None
        for r in results:
            item = claim['item'][r['index']]
            seq = item.get('sequence')
            dec = r['decision']
            certified = D[dec]['certified']
            e = []
            trace = ext_value(item, PAS + 'extension-itemTraceNumber', 'valueIdentifier') or {'system': SID_PAYER + 'item-trace', 'value': f'NSHP-{ident_value}-{seq}'}
            e.append({'url': PAS + 'extension-itemTraceNumber', 'valueIdentifier': copy.deepcopy(trace)})
            if certified:
                e.append({'url': PAS + 'extension-itemPreAuthIssueDate', 'valueDate': self.today()})
                e.append({'url': PAS + 'extension-itemPreAuthPeriod', 'valuePeriod': dict(pre_auth_period)})
            prev = ext_value(item, PAS + 'extension-authorizationNumber', 'valueString')
            current_number = auth_number(seq) if certified else None
            if prev:
                if operation == 'inquire' and prev in replaced:
                    e.append({'url': PAS + 'extension-authorizationNumber', 'valueString': replaced[prev]})
                    current_number = replaced[prev] if certified else current_number
                else:
                    e.append({'url': PAS + 'extension-authorizationNumber', 'valueString': prev})
            admref = ext_value(item, PAS + 'extension-administrationReferenceNumber', 'valueString') or \
                self.rules['administrationReferenceNumberTemplate'].format(claimIdentifierValue=ident_value, itemSequence=seq)
            e.append({'url': PAS + 'extension-administrationReferenceNumber', 'valueString': admref})
            if item.get('servicedPeriod'):
                e.append({'url': PAS + 'extension-itemRequestedServiceDate', 'valuePeriod': copy.deepcopy(item['servicedPeriod'])})
            elif item.get('servicedDate'):
                e.append({'url': PAS + 'extension-itemRequestedServiceDate', 'valueDateTime': item['servicedDate']})
            provider_ref = None
            for cts in item.get('careTeamSequence', []) or []:
                ct = care_team.get(cts)
                if ct and ext_value(ct, PAS + 'extension-careTeamClaimScope', 'valueBoolean') is False:
                    provider_ref = resolve(ct['provider']['reference'], claim_url)
            if not provider_ref and overall:
                provider_ref = resolve(overall['provider']['reference'], claim_url)
            if not provider_ref:
                provider_ref = claim_provider
            if certified:
                e.append({'url': PAS + 'extension-itemAuthorizedProvider', 'extension': [{'url': 'provider', 'valueReference': {'reference': provider_ref}}]})
            if dec == 'modified':
                spec = r['rule']['addItem']
                e.append({'url': PAS + 'extension-itemAuthorizedDetail', 'extension': [
                    {'url': 'productOrServiceCode', 'valueCodeableConcept': {'coding': [spec['productOrService']]}},
                    {'url': 'modifier', 'valueCodeableConcept': {'coding': [spec['modifier']]}},
                    {'url': 'unitPrice', 'valueMoney': {'value': spec['unitPriceUSD'], 'currency': 'USD'}},
                    {'url': 'quantity', 'valueQuantity': ({'value': spec['quantity'], 'unit': 'Units', 'system': X12 + '673', 'code': 'FL'}
                                                          if v221 else {'value': spec['quantity'], 'unit': 'Units'})}]})
            if v221 and admission:
                value = {'valuePeriod': copy.deepcopy(admission['timingPeriod'])} if admission.get('timingPeriod') else {'valueDate': admission.get('timingDate')}
                e.append(dict({'url': PAS + 'extension-admissionDates'}, **value))
            if v221 and discharge and discharge.get('timingDate'):
                e.append({'url': PAS + 'extension-dischargeDate', 'valueDate': discharge['timingDate']})
            for dseq in item.get('diagnosisSequence', []) or []:
                diag = diagnoses.get(dseq)
                if diag and diag.get('diagnosisCodeableConcept'):
                    e.append({'url': PAS + 'extension-communicatedDiagnosis', 'valueCodeableConcept': copy.deepcopy(diag['diagnosisCodeableConcept'])})
                    break
            reason = r['rule'].get('reviewActionReasonCode')
            items.append({'extension': e, 'itemSequence': seq, 'noteNumber': [1],
                          'adjudication': [adjudication(dec, current_number, reason)]})
            if dec == 'modified':
                spec = r['rule']['addItem']
                practitioner_ref = role_practitioner(provider_ref)
                location_ref = role_location(provider_ref)
                ai = {'itemSequence': [seq]}
                if v221:
                    ae = [{'url': PAS + 'extension-itemTraceNumber', 'valueIdentifier': copy.deepcopy(trace)},
                          {'url': PAS + 'extension-itemPreAuthIssueDate', 'valueDate': self.today()},
                          {'url': PAS + 'extension-itemPreAuthPeriod', 'valuePeriod': dict(pre_auth_period)},
                          {'url': PAS + 'extension-authorizationNumber', 'valueString': prev or auth_number(seq)},
                          {'url': PAS + 'extension-administrationReferenceNumber', 'valueString': admref},
                          {'url': PAS + 'extension-epsdtIndicator', 'valueBoolean': False},
                          {'url': PAS + 'extension-nursingHomeResidentialStatus', 'valueCodeableConcept': {'coding': [{'system': X12 + '1345', 'code': '2'}]}},
                          {'url': PAS + 'extension-nursingHomeLevelOfCare', 'valueCodeableConcept': {'coding': [{'system': X12 + '1337', 'code': '2'}]}},
                          {'url': PAS + 'extension-productOrServiceCodeEnd', 'valueCodeableConcept': {'coding': [spec['productOrService']]}},
                          {'url': PAS + 'extension-revenueCode', 'valueCodeableConcept': {'coding': [{'system': 'https://www.nubc.org/CodeSystem/RevenueCodes', 'code': '0292'}]}},
                          {'url': PAS + 'extension-revenueUnitRateLimit', 'valueDecimal': spec['unitPriceUSD']}]
                    source_item = r.get('stored_item') or item
                    requested = ext_value(source_item, PAS + 'extension-requestedService', 'valueReference')
                    if requested:
                        ae.append({'url': PAS + 'extension-requestedService',
                                   'valueReference': {'reference': resolve(requested['reference'], r.get('stored_url') or claim_url)}})
                    for url in ('extension-serviceItemRequestType', 'extension-certificationType'):
                        value = ext_value(item, PAS + url, 'valueCodeableConcept')
                        if value:
                            ae.append({'url': PAS + url, 'valueCodeableConcept': copy.deepcopy(value)})
                    if admission:
                        ae.append(dict({'url': PAS + 'extension-admissionDates'}, **({'valuePeriod': copy.deepcopy(admission['timingPeriod'])} if admission.get('timingPeriod') else {'valueDate': admission.get('timingDate')})))
                    if discharge and discharge.get('timingDate'):
                        ae.append({'url': PAS + 'extension-dischargeDate', 'valueDate': discharge['timingDate']})
                    ai['extension'] = ae
                    if practitioner_ref:
                        ai['provider'] = [{'extension': [{'url': PAS + 'extension-authorizedProviderType', 'valueCodeableConcept': {'coding': [{'system': X12 + '98', 'code': 'SJ'}]}}],
                                           'reference': practitioner_ref}]
                ai['productOrService'] = {'coding': [spec['productOrService']]}
                ai['modifier'] = [{'coding': [spec['modifier']]}]
                ai['servicedPeriod'] = dict(pre_auth_period)
                if v221 and location_ref:
                    ai['locationReference'] = {'reference': location_ref}
                ai['quantity'] = {'value': spec['quantity'], 'unit': 'Units', 'system': X12 + '673', 'code': 'FL'} if v221 else {'value': spec['quantity']}
                ai['unitPrice'] = {'value': spec['unitPriceUSD'], 'currency': 'USD'}
                ai['adjudication'] = [adjudication(spec['decision'], prev or auth_number(seq))]
                add_items.append(ai)
            if r['rule'].get('addError'):
                spec = r['rule']['addError']
                ee = []
                if v221:
                    ee.append({'url': PAS + 'extension-errorElement', 'extension': [{'url': 'processNote', 'valuePositiveInt': 1},
                                                                                   {'url': 'error', 'valueString': spec['errorElement']}]})
                else:
                    ee.append({'url': PAS + 'extension-errorElement', 'valueString': spec['errorElement']})
                ee.append({'url': PAS + 'extension-errorPath', 'valueString': spec['errorPathTemplate'].format(itemIndex=r['index'])})
                ee.append({'url': PAS + 'extension-errorFollowupAction', 'valueCodeableConcept': {'coding': [spec['followupAction']]}})
                errors.append({'extension': ee, 'itemSequence': seq, 'code': {'coding': [spec['code']]}})
            if dec == 'pended' and r['rule'].get('requestAdditionalInformation'):
                if operation == 'submit':
                    comm_request_needed = (r, item)
                task_needed = (r, item)

        patient_ref = resolve(claim['patient']['reference'], claim_url)
        insurer_ref = resolve(claim['insurer']['reference'], claim_url)
        cr = {'resourceType': 'ClaimResponse', 'id': cr_id,
              'meta': {'profile': [PAS + ('profile-claimresponse' if operation == 'submit' else 'profile-claiminquiryresponse')]}}
        if v221:
            sender = ext_value(claim, PAS + 'extension-TransmissionIdentifiers', 'extension') or []
            sub = {x.get('url'): x.get('valueString') for x in sender}
            cr['extension'] = [
                {'url': PAS + 'extension-TransmissionIdentifiers', 'extension': [
                    {'url': 'applicationSenderCode', 'valueString': sub.get('applicationReceiverCode') or self.rules['payer']['applicationSenderCode']},
                    {'url': 'applicationReceiverCode', 'valueString': sub.get('applicationSenderCode') or 'REQUESTOR'}]
                    + ([{'url': 'interchangeSenderID', 'valueString': sub['interchangeReceiverID']}] if sub.get('interchangeReceiverID') else [])
                    + ([{'url': 'interchangeReceiverID', 'valueString': sub['interchangeSenderID']}] if sub.get('interchangeSenderID') else [])},
                {'url': PAS + 'extension-claimResponseReviewer', 'extension': [
                    {'url': 'wasHumanReviewedFlag', 'valueBoolean': any(x in ('denied', 'modified') for x in decisions) or any(r.get('finalized') for r in results)},
                    {'url': 'reviewerNPI', 'valueIdentifier': {'system': NPI_SYS, 'value': self.rules['payer']['reviewerNpi']}}]},
                {'url': PAS + 'extension-itemAuthorizedProvider', 'extension': [
                    {'url': 'provider', 'valueReference': {'reference': resolve(overall['provider']['reference'], claim_url) if overall else claim_provider}}]},
            ]
        cr['identifier'] = copy.deepcopy(claim.get('identifier'))
        cr['status'] = 'active'
        cr['type'] = copy.deepcopy(claim.get('type'))
        cr['use'] = 'preauthorization'
        cr['patient'] = {'reference': patient_ref}
        cr['created'] = self.instant()
        cr['insurer'] = {'reference': insurer_ref}
        cr['requestor'] = {'reference': claim_provider}
        if claim_url and claim_url.startswith(('http://', 'https://')):
            cr['request'] = {'reference': claim_url}
        else:
            cr['request'] = {'extension': [{'url': DAR_URL, 'valueCode': 'unknown'}]}
        if errors:
            cr['outcome'] = 'partial'
        elif claim_decision == 'pended' and not v221:
            cr['outcome'] = 'queued'
        else:
            cr['outcome'] = 'complete'
        cr['preAuthPeriod'] = dict(pre_auth_period)

        patient_res = (entries_by_url.get(patient_ref) or {}).get('resource') or {}
        requestor_res = (entries_by_url.get(claim_provider) or {}).get('resource') or {}
        requestor_npi = next((i.get('value') for i in requestor_res.get('identifier', []) if i.get('system') == NPI_SYS), 'UNKNOWN')

        if comm_request_needed:
            r, item = comm_request_needed
            spec = r['rule']['requestAdditionalInformation']
            crq_id = self.new_id('communicationrequest')
            crq_url = f'urn:uuid:{crq_id}'
            payload_ext = []
            if not v221:
                payload_ext.append({'url': PAS + 'extension-serviceLineNumber', 'valuePositiveInt': item['sequence']})
            dseq = (item.get('diagnosisSequence') or [None])[0]
            if dseq in diagnoses:
                payload_ext.append({'url': PAS + 'extension-communicatedDiagnosis', 'valueCodeableConcept': copy.deepcopy(diagnoses[dseq]['diagnosisCodeableConcept'])})
            payload_ext.append({'url': PAS + 'extension-contentModifier', 'valueCodeableConcept': {'coding': [spec['contentModifier']]}})
            crq = {'resourceType': 'CommunicationRequest', 'id': crq_id, 'meta': {'profile': [PAS + 'profile-communicationrequest']}}
            if v221:
                crq['extension'] = [{'url': PAS + 'extension-serviceLineNumber', 'valuePositiveInt': item['sequence']}]
            crq.update({
                'identifier': [{'system': SID_PAYER + 'communication-request-id', 'value': f"CR-{ident_value}-{item['sequence']}"}],
                'status': 'active',
                'category': [{'coding': [spec['communicationRequestCategory']]}],
                'medium': [{'coding': [{'system': 'http://terminology.hl7.org/CodeSystem/v3-ParticipationMode', 'code': 'ELECTRONIC'}]}],
                'subject': {'reference': patient_ref},
                'payload': [{'extension': payload_ext, 'contentString': spec['attachmentLoinc']['code']}],
                'authoredOn': self.instant(),
                'requester': {'reference': insurer_ref},
                'recipient': [{'reference': claim_provider}],
                'sender': {'reference': claim_provider},
            })
            generated[crq_url] = {'fullUrl': crq_url, 'resource': crq}
            cr['communicationRequest'] = [{'reference': crq_url}]
        cr['item'] = items
        if add_items:
            cr['addItem'] = add_items
        # Header adjudication: reviewAction is must support here, but extension-reviewActionCode is only allowed on
        # item.adjudication and addItem.adjudication (PAS 2.2.1 extension context), so the claim level decision is
        # carried by the items and this reviewAction holds only number and secondSurgicalOpinionFlag.
        header_review = []
        if D[claim_decision]['certified']:
            header_review.append({'url': 'number', 'valueString': self.rules['claimAuthorizationNumberTemplate'].format(claimIdentifierValue=ident_value)})
        header_review.append({'url': 'secondSurgicalOpinionFlag', 'valueBoolean': False})
        cr['adjudication'] = [{'extension': [{'url': PAS + 'extension-reviewAction', 'extension': header_review}],
                               'category': copy.deepcopy(ADJ_CATEGORY)}]
        cr['processNote'] = [{'number': 1, 'type': 'display',
                              'text': f"Synthetic adjudication: claim decision {D[claim_decision]['code']} {D[claim_decision]['display']}."}]
        if errors:
            cr['error'] = errors

        task = None
        if task_needed:
            r, item = task_needed
            spec = r['rule']['requestAdditionalInformation']
            task_id = self.new_id('task')
            reason_reference = {'reference': claim_url} if claim_url and claim_url.startswith('http') else \
                {'identifier': {k: v for k, v in (claim.get('identifier') or [{}])[0].items() if k in ('system', 'value')}}
            line_ext = PAS + ('extension-serviceLineNumber' if v221 else 'extension-paLineNumber')
            line_value = {'valuePositiveInt': item['sequence']} if v221 else {'valueInteger': item['sequence']}
            inputs = [
                {'type': {'coding': [{'system': PASTEMP, 'code': 'payer-url'}]}, 'valueUrl': self.base},
                {'extension': [dict({'url': line_ext}, **line_value)] + ([{'url': PAS + 'extension-contentModifier', 'valueCodeableConcept': {'coding': [spec['contentModifier']]}}] if v221 else []),
                 'type': {'coding': [{'system': PASTEMP, 'code': 'attachments-needed'}]},
                 'valueCodeableConcept': {'coding': [{'system': LOINC, 'code': spec['attachmentLoinc']['code']}]}},
            ]
            if v221:
                inputs.append({'extension': [dict({'url': line_ext}, **line_value)],
                               'type': {'coding': [{'system': PASTEMP, 'code': 'questionnaire-context'}]},
                               'valueString': f"{self.base}/Questionnaire/pas-home-oxygen-documentation"})
            task = {'resourceType': 'Task', 'id': task_id, 'meta': {'profile': [PAS + 'profile-task']},
                    'identifier': [{'system': SID_PAYER + 'task-id', 'value': f"TASK-{ident_value}-{item['sequence']}"}],
                    'status': 'requested',
                    'statusReason': {'text': 'Additional documentation is required to complete the prior authorization review'},
                    'intent': 'order',
                    'code': {'coding': [{'system': PASTEMP, 'code': 'attachment-request-code'}]},
                    'for': {'reference': patient_ref},
                    'authoredOn': self.instant(),
                    'requester': {'identifier': {'system': SID_PAYER + 'payer-id', 'value': 'NSHP01'}},
                    'owner': {'identifier': {'system': NPI_SYS, 'value': requestor_npi}},
                    'reasonCode': {'coding': [{'system': PASTEMP, 'code': 'priorAuthorization'}]},
                    'reasonReference': reason_reference,
                    'restriction': {'period': {'start': self.today(), 'end': self.today(30)}},
                    'input': inputs}

        # Bundle closure: ClaimResponse first, then every resource reachable from it (request excluded),
        # then generated resources. References always equal entry fullUrls.
        out_entries = [{'fullUrl': cr_url, 'resource': cr}]
        included = {cr_url}

        def include(url):
            if not url or url in included:
                return
            if url in generated:
                included.add(url)
                out_entries.append(generated[url])
                for ref in iter_references(generated[url]['resource']):
                    include(resolve(ref, url))
                return
            entry = entries_by_url.get(url)
            if not entry or (entry.get('resource') or {}).get('resourceType') == 'Claim':
                return
            included.add(url)
            out_entries.append({'fullUrl': url, 'resource': copy.deepcopy(entry['resource'])})
            for ref in iter_references(entry['resource']):
                include(resolve(ref, url))

        for ref in iter_references(cr):
            include(resolve(ref, claim_url))
        if task:
            task_url = f'urn:uuid:{task["id"]}'
            include(patient_ref)
            out_entries.append({'fullUrl': task_url, 'resource': task})
        _ = patient_res
        bundle_id = self.new_id('bundle')
        return {'resourceType': 'Bundle', 'id': bundle_id,
                'meta': {'profile': [PAS + ('profile-pas-response-bundle' if operation == 'submit' else 'profile-pas-inquiry-response-bundle')]},
                'identifier': {'system': 'urn:ietf:rfc:3986', 'value': f'urn:uuid:{self.new_id("bundle-identifier")}'},
                'type': 'collection', 'timestamp': self.instant(), 'entry': out_entries}


# ----------------------------------------------------------------------------------------------
# Notification checks (subscriptions_test_kit 0.12.2 common/notification_conformance_verification.rb
# and cross_suite/pas_notification_verification.rb, non-validator parts)
# ----------------------------------------------------------------------------------------------
def context_lint(bundle, nested=False):
    """Validator rules confirmed against the live kit (spec/SPEC.md 16) that pas_checks cannot see."""
    errors = []
    for e in bundle.get('entry', []):
        cr = e.get('resource') or {}
        if cr.get('resourceType') != 'ClaimResponse':
            continue
        for adj in cr.get('adjudication', []):
            for x in adj.get('extension', []):
                if any(s.get('url') == PAS + 'extension-reviewActionCode' for s in x.get('extension', [])):
                    errors.append('ClaimResponse.adjudication reviewAction must not contain extension-reviewActionCode')
        for it in (cr.get('item') or []) + (cr.get('addItem') or []):
            for adj in it.get('adjudication', []):
                for x in adj.get('extension', []):
                    for s in x.get('extension', []):
                        if s.get('url') == 'reasonCode':
                            systems = {c.get('system') for c in s['valueCodeableConcept'].get('coding', [])}
                            if systems != {'https://codesystem.x12.org/external/886'}:
                                errors.append(f'reviewAction reasonCode must use https://codesystem.x12.org/external/886, got {systems}')
            for x in it.get('extension', []):
                if x.get('url') == PAS + 'extension-itemAuthorizedDetail':
                    for s in x.get('extension', []):
                        q = s.get('valueQuantity')
                        if q is not None and not (q.get('unit') or q.get('code')):
                            errors.append('itemAuthorizedDetail quantity needs unit or code (prof-2)')
        if nested:
            omit = PasModel.NOTIFICATION_OMIT
            if any(x.get('url') in omit['root'] for x in cr.get('extension', [])):
                errors.append('notification ClaimResponse must omit root extension-itemAuthorizedProvider')
            for key in ('item', 'addItem'):
                for it in cr.get(key, []) or []:
                    bad = [x['url'] for x in it.get('extension', []) if x.get('url') in omit[key]]
                    if bad:
                        errors.append(f'notification ClaimResponse.{key} must omit {bad}')
    return errors


def check_notification(message, kind, content, subscription):
    errors = []
    body = message['body']
    if body.get('type') != 'history':
        errors.append(f'{kind}: notification Bundle.type must be history')
    entries = body.get('entry') or []
    if not entries:
        return errors + [f'{kind}: notification Bundle is empty']
    status_entry = entries[0]
    sid = subscription['id']
    for i, e in enumerate(entries):
        if not e.get('request') or not e.get('response'):
            errors.append(f'{kind}: entry {i + 1} must have request and response')
    if not (status_entry.get('request') or {}).get('url', '').endswith(f'Subscription/{sid}/$status'):
        errors.append(f'{kind}: SubscriptionStatus request.url must end with Subscription/{sid}/$status')
    params = status_entry.get('resource') or {}
    if params.get('resourceType') != 'Parameters':
        return errors + [f'{kind}: first entry must be Parameters']
    by_name = {}
    for p in params.get('parameter', []):
        by_name.setdefault(p['name'], []).append(p)
    ptype = by_name.get('type', [{}])[0].get('valueCode')
    pstatus = by_name.get('status', [{}])[0].get('valueCode')
    if ptype != kind:
        errors.append(f'{kind}: type parameter is {ptype}')
    expected_status = 'requested' if kind == 'handshake' else 'active'
    if pstatus != expected_status:
        errors.append(f'{kind}: status parameter must be {expected_status}, got {pstatus}')
    sub_ref = by_name.get('subscription', [{}])[0].get('valueReference', {}).get('reference', '')
    if sub_ref.split('/')[-1] != sid:
        errors.append(f'{kind}: subscription reference last segment must be the Subscription id {sid}')
    if kind == 'event-notification':
        mime = (subscription.get('channel') or {}).get('payload')
        if mime and not message['headers'].get('Content-Type', '').startswith(mime):
            errors.append('event-notification: Content-Type must start with the Subscription channel.payload')
        for h in (subscription.get('channel') or {}).get('header', []) or []:
            name, _, value = h.partition(': ')
            if message['headers'].get(name) != value:
                errors.append(f'event-notification: header {name} must equal {value!r}')
        events = by_name.get('notification-event', [])
        if not events:
            errors.append('event-notification: notification-event parameter required')
        rest = entries[1:]
        for ev in events:
            focus = next((p for p in ev.get('part', []) if p['name'] == 'focus'), None)
            if not focus:
                errors.append('event-notification: focus required')
                continue
            ref = focus['valueReference']['reference']
            target = next((e for e in rest if e.get('fullUrl') == ref or (e.get('resource') and ref.endswith(f"{e['resource']['resourceType']}/{e['resource'].get('id')}"))), None)
            if content == 'full-resource':
                if not target:
                    errors.append('event-notification: focus must resolve to a Bundle entry')
                    continue
                res = target.get('resource') or {}
                if res.get('resourceType') != 'Bundle' or res.get('type') != 'collection':
                    errors.append('event-notification: focus must be a PAS Response Bundle (collection)')
                elif not any((e.get('resource') or {}).get('resourceType') == 'ClaimResponse' for e in res.get('entry', [])):
                    errors.append('event-notification: focus Bundle must contain a ClaimResponse')
            else:
                for e in rest:
                    if e.get('resource'):
                        errors.append('id-only: entries must not contain resource')
                    if not e.get('fullUrl'):
                        errors.append('id-only: entries must contain fullUrl')
    return errors


# ----------------------------------------------------------------------------------------------
# Replay
# ----------------------------------------------------------------------------------------------
def load(path):
    with open(path) as f:
        return json.load(f)


def write(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, 'w') as f:
        json.dump(data, f, indent=2)
        f.write('\n')


def kit_error_bundle(version, kit_dir):
    if kit_dir:
        path = os.path.join(kit_dir, 'lib', 'davinci_pas_test_kit', 'server', 'v2.2.1' if version == 'v221' else 'v2.0.1',
                            'error_tests', 'nonconformant_pas_bundle.json')
        if not os.path.exists(path):
            raise SystemExit(f'kit error bundle not found: {path} (check --kit-dir or PAS_TEST_KIT_DIR)')
        return load(path)
    return {'resourceType': 'Bundle', 'type': 'collection', 'entry': []}


def replay(version, write_examples=True, kit_dir=None):
    td = os.path.join(TESTDATA, version)
    ex = os.path.join(SPEC_DIR, 'examples', version)
    rules = load(os.path.join(SPEC_DIR, 'adjudication-rules.json'))
    expected = load(os.path.join(td, 'expected.json'))
    fhir_base = f'http://host.docker.internal:8085/{version}/fhir'
    model = PasModel(version, fhir_base, rules)
    failures = []
    submit_reqs, submit_resps, inquire_reqs, inquire_resps = [], [], [], []

    def fail(label, errs):
        for e in errs:
            failures.append(f'{version} {label}: {e}')

    def out(name, data):
        if write_examples:
            write(os.path.join(ex, name), data)

    def decisions_of(bundle):
        items = {}
        for e in bundle.get('entry', []):
            r = e.get('resource') or {}
            if r.get('resourceType') == 'ClaimResponse':
                for it in r.get('item', []):
                    for adj in it.get('adjudication', []):
                        for x in adj.get('extension', []):
                            for s in x.get('extension', []):
                                if s.get('url') == PAS + 'extension-reviewActionCode':
                                    items[str(it['itemSequence'])] = s['valueCodeableConcept']['coding'][0]['code']
        return items

    def expect_items(label, bundle, exp_items):
        got = decisions_of(bundle)
        want = {str(k): v for k, v in exp_items.items()}
        if got != want:
            failures.append(f'{version} {label}: item decisions {got} != expected {want}')

    def do_submit(label, req, exp=None, use_case=None, echo_name=None):
        fail(label + ' request structure', checks.check_request_structure(req, 'submit'))
        status, resp = model.submit(req)
        if status != 200:
            failures.append(f'{version} {label}: status {status}')
            return None
        submit_reqs.append(req)
        submit_resps.append(resp)
        fail(label + ' response structure', checks.check_response_structure(resp, req, 'submit'))
        fail(label + ' validator context rules', context_lint(resp))
        if use_case:
            fail(label + ' decision', checks.check_decision(resp, use_case))
        if exp:
            expect_items(label, resp, exp['items'])
        if echo_name:
            out(echo_name, resp)
        return resp

    def do_inquire(label, req, exp=None, echo_name=None):
        fail(label + ' request structure', checks.check_request_structure(req, 'inquire'))
        status, resp = model.inquire(req)
        if status != 200:
            failures.append(f'{version} {label}: status {status}')
            return None
        inquire_reqs.append(req)
        inquire_resps.append(resp)
        bundles = checks.bundles_from_body(resp)
        if version == 'v221' and resp.get('resourceType') != 'Parameters':
            failures.append(f'{version} {label}: v2.2.1 $inquire must return Parameters')
        if version == 'v201' and resp.get('resourceType') != 'Bundle':
            failures.append(f'{version} {label}: v2.0.1 $inquire must return a Bundle')
        for b in bundles:
            fail(label + ' response structure', checks.check_response_structure(b, req, 'inquire'))
            fail(label + ' validator context rules', context_lint(b))
            if exp:
                expect_items(label, b, exp['items'])
        if echo_name:
            out(echo_name, resp)
        return resp

    # 0. CapabilityStatement
    cs = model.metadata()
    out('metadata.json', cs)
    sub_rest = next(r for r in cs['rest'] if r['mode'] == 'server')
    sub_res = next(r for r in sub_rest['resource'] if r['type'] == 'Subscription')
    if {'create', 'update', 'delete'} - {i['code'] for i in sub_res['interaction']}:
        failures.append(f'{version} metadata: Subscription must declare create, update, delete')

    # 1. Subscription Setup (Inferno rewrites endpoint and appends the Authorization header)
    sub_in = load(os.path.join(td, 'subscription.json'))
    fail('subscription input', checks.check_subscription(sub_in, version))
    token = f'nirmitee-pas-{version}-example-token'
    sub_sent = copy.deepcopy(sub_in)
    sub_sent['channel']['endpoint'] = 'http://localhost:8280/custom/subscriptions_r5_backport_r4_server/subscription/channel/notification_listener'
    sub_sent['channel']['header'] = [f'Authorization: Bearer {token}']
    status, sub_created = model.create_subscription(sub_sent)
    if status != 201 or sub_created.get('status') != 'requested':
        failures.append(f'{version} subscription create: expected 201 with status requested')
    out('subscription-create-request.json', sub_sent)
    out('subscription-create-response.json', sub_created)
    handshake = model.outbox.pop(0)
    fail('handshake', check_notification(handshake, 'handshake', None, sub_created))
    out('notification-handshake.json', {'headers': handshake['headers'], 'body': handshake['body']})

    # 2. Workflows
    for name, use_case in (('approval_submit', 'approval'), ('denial_submit', 'denial'), ('pended_submit', 'pended')):
        req = load(os.path.join(td, name + '.json'))
        do_submit(name, req, expected[name], use_case, name.replace('_submit', '') + '-submit-response.json')
        if name == 'approval_submit':
            custom = checks.no_custom_extensions(req)
            if custom:
                failures.append(f'{version} approval_submit: custom extensions {custom}')
    model.finalize_pended()
    events = [m for m in model.outbox if m['kind'] == 'event-notification']
    model.outbox = [m for m in model.outbox if m['kind'] != 'event-notification']
    if len(events) != 1:
        failures.append(f'{version} finalize: expected 1 event notification, got {len(events)}')
    for m in events:
        fail('event-notification', check_notification(m, 'event-notification', 'full-resource' if version == 'v221' else 'id-only', sub_created))
        out('notification-event.json', {'headers': m['headers'], 'body': m['body']})
        if version == 'v221':
            focus_bundle = m['body']['entry'][1]['resource']
            fail('event-notification validator context rules', context_lint(focus_bundle, nested=True))
            expect_items('pended finalized notification', focus_bundle, expected['pended_submit']['after_finalize'])
    if version == 'v201':
        req = load(os.path.join(td, 'pended_inquire.json'))
        do_inquire('pended_inquire', req, expected['pended_inquire'], 'pended-inquire-response.json')

    if version == 'v221':
        steps = ['claim_update_initial_submit', 'claim_update_add_item_submit', 'claim_update_modify_cancel_submit', 'claim_update_cancel_all_submit']
        for step in steps:
            req = load(os.path.join(td, step + '.json'))
            resp = do_submit(step, req, expected[step], None, step.replace('_submit', '').replace('_', '-') + '-response.json')
            if resp:
                fail(step + ' item sequences', checks.check_claim_update_sequences(req, resp, step))

    # 3. Must support
    ms_submit = load(os.path.join(td, 'must_support_submit.json'))
    for i, (req, exp) in enumerate(zip(ms_submit, expected['must_support_submit'])):
        do_submit(f'must_support_submit[{i}]', req, exp, None, f'must-support-submit-response-{i + 1}.json')
    ms_inquire = load(os.path.join(td, 'must_support_inquire.json'))
    ms_inquire_resps = []
    for i, (req, exp) in enumerate(zip(ms_inquire, expected['must_support_inquire'])):
        ms_inquire_resps.append(do_inquire(f'must_support_inquire[{i}]', req, exp, f'must-support-inquire-response-{i + 1}.json'))
    fail('replaced reference (spec-47)', checks.check_replaced_reference(ms_inquire, ms_inquire_resps))
    if version == 'v221':
        fail('inquiry claim reference (spec-44)', checks.check_inquiry_claim_reference(inquire_resps))

    fail('MS submit requests', checks.check_must_support(version, 'submit_request', submit_reqs))
    fail('MS submit responses', checks.check_must_support(version, 'submit_response', submit_resps))
    fail('MS inquire requests', checks.check_must_support(version, 'inquire_request', inquire_reqs))
    fail('MS inquire responses', checks.check_must_support(version, 'inquire_response', inquire_resps))

    # 4. Error handling: the kit posts an empty collection Bundle to $submit and $inquire
    empty = kit_error_bundle(version, kit_dir)
    for op in ('submit', 'inquire'):
        status, oo = getattr(model, op)(empty)
        if not (400 <= status < 500) or oo.get('resourceType') != 'OperationOutcome':
            failures.append(f'{version} error {op}: expected 4xx OperationOutcome, got {status}')
        out(f'error-{op}-response.json', {'status': status, 'body': oo})
    return failures


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument('--version', default='all', choices=['v221', 'v201', 'all'])
    ap.add_argument('--no-write', action='store_true')
    ap.add_argument('--kit-dir', default=os.environ.get('PAS_TEST_KIT_DIR'),
                    help='davinci-pas-test-kit clone (default: PAS_TEST_KIT_DIR)')
    args = ap.parse_args()
    versions = ['v221', 'v201'] if args.version == 'all' else [args.version]
    all_failures = []
    for v in versions:
        f = replay(v, not args.no_write, args.kit_dir)
        print(f'{v}: {"PASS" if not f else "FAIL"} ({len(f)} issue(s))')
        for line in f:
            print('  ' + line)
        all_failures += f
    return 1 if all_failures else 0


if __name__ == '__main__':
    sys.exit(main())
