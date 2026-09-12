#!/usr/bin/env python3
"""Build the synthetic PAS server testdata for the Inferno Da Vinci PAS Server suites v2.2.1 and v2.0.1.

Every person, organization, identifier and clinical detail here is synthetic test data.
Output: testdata/v221/*.json, testdata/v201/*.json, each with inputs.json (Inferno input name to
file/value mapping, consumed by harness/run_suite.py) and expected.json (expected decisions per
item, consumed by server unit tests and spec/reference/pas_reference.py).

Design notes (see spec/SPEC.md section 9):
  - every Reference.reference equals the fullUrl of an entry in the same Bundle (absolute URLs or urn:uuid);
  - decisions are driven by Claim.item.productOrService HCPCS codes listed in spec/adjudication-rules.json;
  - must support coverage is aggregated by Inferno across all requests of a session, so the rich
    must support bundles carry every must support element while workflow bundles stay small.

Usage: python3 testdata/build_testdata.py
Python 3 stdlib only.
"""
import base64
import copy
import hashlib
import json
import os

HERE = os.path.dirname(os.path.abspath(__file__))

BASE = 'https://synthetic.nirmitee.io/fhir'
SID = 'https://synthetic.nirmitee.io/fhir/sid/'
PAS = 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/'
US_CORE = 'http://hl7.org/fhir/us/core/StructureDefinition/'
X12 = 'https://codesystem.x12.org/005010/'
HCPCS = 'http://www.cms.gov/Medicare/Coding/HCPCSReleaseCodeSets'
POS = 'https://www.cms.gov/Medicare/Coding/place-of-service-codes/Place_of_Service_Code_Set'
NUBC_REV = 'https://www.nubc.org/CodeSystem/RevenueCodes'
PASTEMP = 'http://hl7.org/fhir/us/davinci-pas/CodeSystem/PASTempCodes'
ICD10 = 'http://hl7.org/fhir/sid/icd-10-cm'
SNOMED = 'http://snomed.info/sct'
LOINC = 'http://loinc.org'
NUCC = 'http://nucc.org/provider-taxonomy'
NPI_SYS = 'http://hl7.org/fhir/sid/us-npi'
TIN_SYS = 'http://terminology.hl7.org/NamingSystem/USEIN'
CRD_COV = 'http://hl7.org/fhir/us/davinci-crd/StructureDefinition/ext-coverage-information'
TOPIC = 'http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic'
BACKPORT = 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/'
CLAIM_ENCOUNTER_EXT = 'http://hl7.org/fhir/5.0/StructureDefinition/extension-Claim.encounter'

TIMESTAMP = '2026-09-01T10:00:00Z'
SERVICE_START = '2026-09-15'
SERVICE_END = '2026-12-14'


def npi(nine):
    """US NPI with Luhn check digit computed over the 80840 prefix."""
    digits = '80840' + nine
    total = 0
    for i, ch in enumerate(reversed(digits)):
        d = int(ch)
        if i % 2 == 0:
            d *= 2
            if d > 9:
                d -= 9
        total += d
    return nine + str((10 - total % 10) % 10)


assert npi('123456789') == '1234567893'
NPI_PAYER = npi('190000000')        # 1900000005
NPI_PROVIDER_ORG = npi('174502300')
NPI_PRACTITIONER = npi('112233445')


def uuid4like(seed):
    h = hashlib.sha1(seed.encode()).hexdigest()
    return f'{h[0:8]}-{h[8:12]}-4{h[13:16]}-a{h[17:20]}-{h[20:32]}'


def cc(system, code, display=None):
    coding = {'system': system, 'code': code}
    if display:
        coding['display'] = display
    return {'coding': [coding]}


def ext(url, **value):
    out = {'url': url}
    out.update(value)
    return out


class Ctx:
    """Per bundle context: resource ids, fullUrls (absolute or urn:uuid) and version switches."""

    def __init__(self, version, scheme, seed):
        self.version = version
        self.scheme = scheme
        self.seed = seed

    @property
    def v221(self):
        return self.version == 'v221'

    def id(self, rtype, lid):
        return lid if self.scheme == 'abs' else uuid4like(f'{self.version}|{self.seed}|{rtype}|{lid}')

    def url(self, rtype, lid):
        rid = self.id(rtype, lid)
        return f'{BASE}/{rtype}/{rid}' if self.scheme == 'abs' else f'urn:uuid:{rid}'

    def ref(self, rtype, lid):
        return {'reference': self.url(rtype, lid)}


# ------------------------------------------------------------------------------------------------
# Shared resources
# ------------------------------------------------------------------------------------------------
def patient(ctx):
    return {
        'resourceType': 'Patient', 'id': ctx.id('Patient', 'pas-pat-001'),
        'meta': {'profile': [PAS + 'profile-beneficiary', PAS + 'profile-subscriber']},
        'extension': [
            {'url': US_CORE + 'us-core-race', 'extension': [
                {'url': 'ombCategory', 'valueCoding': {'system': 'urn:oid:2.16.840.1.113883.6.238', 'code': '2028-9', 'display': 'Asian'}},
                {'url': 'text', 'valueString': 'Asian'}]},
            {'url': US_CORE + 'us-core-ethnicity', 'extension': [
                {'url': 'ombCategory', 'valueCoding': {'system': 'urn:oid:2.16.840.1.113883.6.238', 'code': '2186-5', 'display': 'Not Hispanic or Latino'}},
                {'url': 'text', 'valueString': 'Not Hispanic or Latino'}]},
            {'url': US_CORE + 'us-core-birthsex', 'valueCode': 'F'},
        ],
        'identifier': [{'use': 'usual', 'type': cc('http://terminology.hl7.org/CodeSystem/v2-0203', 'MB'),
                        'system': SID + 'member-id', 'value': 'NSHP-M-000123'}],
        'name': [{'use': 'official', 'family': 'Testwell', 'given': ['Maya', 'R'], 'prefix': ['Ms.'], 'suffix': ['III']}],
        'telecom': [{'system': 'phone', 'value': '555-0100', 'use': 'home'},
                    {'system': 'email', 'value': 'maya.testwell@example.com', 'use': 'home'}],
        'gender': 'female',
        'birthDate': '1958-04-12',
        'address': [{'use': 'home', 'line': ['100 Synthetic Way'], 'city': 'Springfield', 'district': 'Greene',
                     'state': 'MO', 'postalCode': '65801', 'country': 'US', 'period': {'start': '2015-01-01'}}],
        'multipleBirthInteger': 1,
        'communication': [{'language': {'coding': [{'system': 'urn:ietf:bcp:47', 'code': 'en-US'}]}, 'preferred': True}],
    }


def payer_org(ctx):
    return {
        'resourceType': 'Organization', 'id': ctx.id('Organization', 'pas-org-payer'),
        'meta': {'profile': [PAS + 'profile-insurer']},
        'identifier': [
            {'system': NPI_SYS, 'value': NPI_PAYER},
            {'system': TIN_SYS, 'value': '431234567'},
            {'type': cc('http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType', 'payerid'),
             'system': SID + 'payer-id', 'value': 'NSHP01'},
        ],
        'active': True,
        'type': [cc(X12 + '98', 'PR', 'Payer')],
        'name': 'Northwind Synthetic Health Plan',
        'telecom': [{'system': 'phone', 'value': '555-0110', 'use': 'work'}],
        'address': [{'use': 'work', 'line': ['500 Payer Plaza'], 'city': 'Springfield', 'state': 'MO',
                     'postalCode': '65802', 'country': 'US'}],
    }


def provider_org(ctx):
    return {
        'resourceType': 'Organization', 'id': ctx.id('Organization', 'pas-org-provider'),
        'meta': {'profile': [PAS + 'profile-requestor']},
        'identifier': [
            {'system': NPI_SYS, 'value': NPI_PROVIDER_ORG},
            {'system': TIN_SYS, 'value': '437654321'},
            {'type': cc('http://hl7.org/fhir/us/carin-bb/CodeSystem/C4BBIdentifierType', 'payerid'),
             'system': SID + 'trading-partner-id', 'value': 'LSMG01'},
        ],
        'active': True,
        'type': [cc(X12 + '98', '1P', 'Provider')],
        'name': 'Lakeside Synthetic Medical Group',
        'telecom': [{'system': 'phone', 'value': '555-0120', 'use': 'work'}],
        'address': [{'use': 'work', 'line': ['200 Lakeside Drive'], 'city': 'Springfield', 'state': 'MO',
                     'postalCode': '65804', 'country': 'US'}],
        'contact': [{'name': {'family': 'Rao', 'given': ['Asha']},
                     'telecom': [{'system': 'phone', 'value': '555-0121', 'use': 'work'}]}],
    }


def practitioner(ctx):
    return {
        'resourceType': 'Practitioner', 'id': ctx.id('Practitioner', 'pas-prac-001'),
        'meta': {'profile': [PAS + 'profile-practitioner']},
        'identifier': [{'system': NPI_SYS, 'value': NPI_PRACTITIONER}],
        'name': [{'use': 'official', 'family': 'Rao', 'given': ['Asha'], 'prefix': ['Dr.']}],
        'telecom': [{'system': 'phone', 'value': '555-0122', 'use': 'work'}],
        'address': [{'use': 'work', 'line': ['200 Lakeside Drive'], 'city': 'Springfield', 'state': 'MO',
                     'postalCode': '65804', 'country': 'US'}],
    }


def location(ctx):
    return {
        'resourceType': 'Location', 'id': ctx.id('Location', 'pas-loc-001'),
        'meta': {'profile': [PAS + 'profile-location']},
        'status': 'active',
        'name': 'Lakeside Synthetic Sleep Clinic',
        'telecom': [{'system': 'phone', 'value': '555-0124', 'use': 'work'}],
        'address': {'use': 'work', 'line': ['200 Lakeside Drive', 'Suite 3'], 'city': 'Springfield', 'state': 'MO',
                    'postalCode': '65804', 'country': 'US'},
        'managingOrganization': ctx.ref('Organization', 'pas-org-provider'),
    }


def endpoint(ctx):
    return {
        'resourceType': 'Endpoint', 'id': ctx.id('Endpoint', 'pas-endpoint-001'),
        'status': 'active',
        'connectionType': {'system': 'http://terminology.hl7.org/CodeSystem/endpoint-connection-type', 'code': 'hl7-fhir-rest'},
        'name': 'Lakeside Synthetic Medical Group FHIR endpoint',
        'payloadType': [cc('http://terminology.hl7.org/CodeSystem/endpoint-payload-type', 'any')],
        'address': 'https://synthetic.nirmitee.io/provider/fhir',
    }


def practitioner_role(ctx):
    role = {
        'resourceType': 'PractitionerRole', 'id': ctx.id('PractitionerRole', 'pas-role-001'),
        'meta': {'profile': [PAS + 'profile-practitionerrole']},
        'active': True,
        'practitioner': ctx.ref('Practitioner', 'pas-prac-001'),
        'organization': ctx.ref('Organization', 'pas-org-provider'),
        'code': [cc(NUCC, '207RP1001X')],
        'specialty': [cc(NUCC, '207RP1001X')],
        'location': [ctx.ref('Location', 'pas-loc-001')],
        'telecom': [{'system': 'phone', 'value': '555-0123', 'use': 'work'}],
        'endpoint': [ctx.ref('Endpoint', 'pas-endpoint-001')],
    }
    return role


def coverage(ctx):
    return {
        'resourceType': 'Coverage', 'id': ctx.id('Coverage', 'pas-cov-001'),
        'meta': {'profile': [PAS + 'profile-coverage']},
        'identifier': [{'type': cc('http://terminology.hl7.org/CodeSystem/v2-0203', 'MB'),
                        'system': SID + 'member-id', 'value': 'NSHP-M-000123'}],
        'status': 'active',
        'type': cc('https://nahdo.org/sopt', '5'),
        'subscriber': ctx.ref('Patient', 'pas-pat-001'),
        'subscriberId': 'NSHP-M-000123',
        'beneficiary': ctx.ref('Patient', 'pas-pat-001'),
        'relationship': {'coding': [{'system': 'http://terminology.hl7.org/CodeSystem/subscriber-relationship', 'code': 'self'},
                                    {'system': X12 + '1069', 'code': '18', 'display': 'Self'}]},
        'period': {'start': '2026-01-01', 'end': '2026-12-31'},
        'payor': [ctx.ref('Organization', 'pas-org-payer')],
        'class': [
            {'type': cc('http://terminology.hl7.org/CodeSystem/coverage-class', 'group'), 'value': 'GRP-4471', 'name': 'Synthetic Employers Group'},
            {'type': cc('http://terminology.hl7.org/CodeSystem/coverage-class', 'plan'), 'value': 'PLAN-PPO-2', 'name': 'Synthetic PPO Plan'},
        ],
    }


def condition(ctx):
    return {
        'resourceType': 'Condition', 'id': ctx.id('Condition', 'pas-cond-001'),
        # v2.0.1: no declared profile. The kit validates declared US Core profiles as <url>|6.1.0 in both suites, and
        # us-core-condition does not exist in US Core 6.1.0 (validator 500 "Unable to resolve profile", seen live).
        **({'meta': {'profile': [US_CORE + 'us-core-condition-encounter-diagnosis']}} if ctx.v221 else {}),
        'clinicalStatus': cc('http://terminology.hl7.org/CodeSystem/condition-clinical', 'active'),
        'verificationStatus': cc('http://terminology.hl7.org/CodeSystem/condition-ver-status', 'confirmed'),
        'category': [cc('http://terminology.hl7.org/CodeSystem/condition-category', 'encounter-diagnosis')],
        'code': {'coding': [{'system': ICD10, 'code': 'G47.33'}, {'system': SNOMED, 'code': '78275009'}]},
        'subject': ctx.ref('Patient', 'pas-pat-001'),
        'recordedDate': '2026-08-28',
    }


def encounter(ctx):
    return {
        'resourceType': 'Encounter', 'id': ctx.id('Encounter', 'pas-enc-001'),
        'meta': {'profile': [PAS + 'profile-encounter'], 'lastUpdated': '2026-09-01T09:00:00Z'},
        'extension': [
            ext(PAS + 'extension-patientStatus', valueCodeableConcept=cc(X12 + '1352', '1')),
            ext(PAS + 'extension-nursingHomeResidentialStatus', valueCodeableConcept=cc(X12 + '1345', '2')),
        ],
        'identifier': [{'system': SID + 'encounter-id', 'value': 'ENC-2026-0915-01'}],
        'status': 'planned',
        'class': {'system': 'http://terminology.hl7.org/CodeSystem/v3-ActCode', 'code': 'AMB'},
        'type': [cc(X12 + '1315', '3')],
        'subject': ctx.ref('Patient', 'pas-pat-001'),
        'participant': [{'type': [cc('http://terminology.hl7.org/CodeSystem/v3-ParticipationType', 'ATND')],
                         'period': {'start': SERVICE_START, 'end': SERVICE_START},
                         'individual': ctx.ref('Practitioner', 'pas-prac-001')}],
        'period': {'start': SERVICE_START, 'end': SERVICE_START},
        'reasonCode': [{'coding': [{'system': SNOMED, 'code': '78275009'}]}],
        'reasonReference': [ctx.ref('Condition', 'pas-cond-001')],
        'hospitalization': {'admitSource': cc(X12 + '1314', '1'),
                            'dischargeDisposition': cc('http://terminology.hl7.org/CodeSystem/discharge-disposition', 'home')},
        'location': [{'location': ctx.ref('Location', 'pas-loc-001')}],
        'serviceProvider': ctx.ref('Organization', 'pas-org-provider'),
    }


def document_reference(ctx):
    text = 'Synthetic home sleep study summary for PAS testing. Not real patient data.'
    return {
        'resourceType': 'DocumentReference', 'id': ctx.id('DocumentReference', 'pas-doc-001'),
        'meta': {'profile': [PAS + 'profile-documentreference' if ctx.v221 else US_CORE + 'us-core-documentreference']},
        'identifier': [{'system': SID + 'document-id', 'value': 'DOC-2026-0001'}],
        'status': 'current',
        'type': cc(LOINC, '11506-3'),
        'category': [cc('http://hl7.org/fhir/us/core/CodeSystem/us-core-documentreference-category', 'clinical-note')],
        'subject': ctx.ref('Patient', 'pas-pat-001'),
        'date': '2026-08-28T15:00:00Z',
        'author': [ctx.ref('Practitioner', 'pas-prac-001')],
        'content': [{'attachment': {'contentType': 'text/plain', 'data': base64.b64encode(text.encode()).decode(),
                                    'title': 'Home sleep study summary'}}],
    }


def quantity(ctx, value):
    if ctx.v221:
        return {'value': value, 'unit': 'Units', 'system': X12 + '673', 'code': 'FL'}
    return {'value': value}


def service_request(ctx, lid, code, assertion_id, code_end=None):
    sr = {
        'resourceType': 'ServiceRequest', 'id': ctx.id('ServiceRequest', lid),
        'meta': {'profile': [PAS + 'profile-servicerequest']},
        'extension': [
            {'url': CRD_COV, 'extension': [
                {'url': 'coverage', 'valueReference': ctx.ref('Coverage', 'pas-cov-001')},
                {'url': 'covered', 'valueCode': 'covered'},
                {'url': 'pa-needed', 'valueCode': 'auth-needed'},
                {'url': 'date', 'valueDate': '2026-08-30'},
                {'url': 'coverage-assertion-id', 'valueString': assertion_id},
            ]},
        ],
        'status': 'active',
        'intent': 'order',
        'code': cc(HCPCS, code),
        'quantityQuantity': quantity(ctx, 1),
        'subject': ctx.ref('Patient', 'pas-pat-001'),
        'occurrenceTiming': {'repeat': {'boundsDuration': {'value': 90, 'unit': 'days', 'system': 'http://unitsofmeasure.org', 'code': 'd'},
                                        'frequency': 1, 'period': 1, 'periodUnit': 'd'}},
        'authoredOn': '2026-08-30',
        'requester': ctx.ref('Practitioner', 'pas-prac-001'),
    }
    if code_end:
        sr['extension'].append(ext(PAS + 'extension-productOrServiceCodeEnd', valueCodeableConcept=cc(HCPCS, code_end)))
    return sr


# ------------------------------------------------------------------------------------------------
# Claim
# ------------------------------------------------------------------------------------------------
def claim_item(ctx, it, kind, rich):
    e = []
    if it.get('trace'):
        e.append(ext(PAS + 'extension-itemTraceNumber', valueIdentifier={'system': SID + 'item-trace', 'value': it['trace']}))
    if it.get('auth'):
        e.append(ext(PAS + 'extension-authorizationNumber', valueString=it['auth']))
    if it.get('admref'):
        e.append(ext(PAS + 'extension-administrationReferenceNumber', valueString=it['admref']))
    e.append(ext(PAS + 'extension-serviceItemRequestType', valueCodeableConcept=cc(X12 + '1525', 'HS', 'Health Services Review')))
    e.append(ext(PAS + 'extension-certificationType', valueCodeableConcept=cc(X12 + '1322', it.get('cert', 'I'))))
    if it.get('code_end'):
        e.append(ext(PAS + 'extension-productOrServiceCodeEnd', valueCodeableConcept=cc(HCPCS, it['code_end'])))
    if kind != 'inquiry' and it.get('sr'):
        e.append(ext(PAS + 'extension-requestedService', valueReference=ctx.ref('ServiceRequest', it['sr'])))
    if rich and kind != 'inquiry':
        e.append(ext(PAS + 'extension-epsdtIndicator', valueBoolean=False))
        e.append(ext(PAS + 'extension-nursingHomeResidentialStatus', valueCodeableConcept=cc(X12 + '1345', '2')))
        e.append(ext(PAS + 'extension-nursingHomeLevelOfCare', valueCodeableConcept=cc(X12 + '1337', '2')))
        e.append(ext(PAS + 'extension-revenueUnitRateLimit', valueDecimal=it.get('price', 100.0)))
    if rich and kind == 'inquiry':
        e.append(ext(PAS + 'extension-itemCertificationIssueDate', valueDate='2026-09-02'))
        e.append(ext(PAS + 'extension-itemCertificationEffectiveDate', valueDate=SERVICE_START))
        e.append(ext(PAS + 'extension-itemCertificationExpirationDate', valueDate=SERVICE_END))
        e.append(ext(PAS + 'extension-reviewActionCode', valueCodeableConcept=cc(X12 + '306', it.get('last_decision', 'A4'))))
    if it.get('info_changed'):
        e.append(ext(PAS + 'extension-infoChanged', valueCode=it['info_changed']))
    item = {'extension': e}
    if it.get('cancelled'):
        item['modifierExtension'] = [ext(PAS + 'modifierextension-infoCancelled', valueBoolean=True)]
    item['sequence'] = it['seq']
    if rich:
        item['careTeamSequence'] = [1, 2]
        item['diagnosisSequence'] = [1]
        item['informationSequence'] = [1, 2, 3]
        item['revenue'] = cc(NUBC_REV, '0292')
    item['category'] = cc(X12 + '1365', '12', 'Durable Medical Equipment Purchase')
    item['productOrService'] = cc(HCPCS, it['code'])
    if rich:
        item['modifier'] = [cc(HCPCS, 'NU')]
    if it.get('served') == 'period':
        item['servicedPeriod'] = {'start': SERVICE_START, 'end': SERVICE_END}
    else:
        item['servicedDate'] = SERVICE_START
    item['locationCodeableConcept'] = cc(POS, '12')
    item['quantity'] = quantity(ctx, it.get('qty', 1))
    if rich:
        item['unitPrice'] = {'value': it.get('price', 100.0), 'currency': 'USD'}
    return item


def supporting_info(ctx, kind, spec):
    si = [
        {'sequence': 1, 'category': cc(PASTEMP, 'patientEvent'), 'timingPeriod': {'start': SERVICE_START, 'end': SERVICE_END}},
        {'sequence': 2, 'category': cc(PASTEMP, 'admissionDates'), 'timingPeriod': {'start': SERVICE_START, 'end': '2026-09-16'}},
        {'sequence': 3, 'category': cc(PASTEMP, 'dischargeDates'), 'timingDate': '2026-09-16'},
    ]
    if kind != 'inquiry':
        si.append({
            'extension': [{'url': PAS + 'extension-documentInformation', 'extension': [
                {'url': 'reportTypeCode', 'valueCodeableConcept': cc(X12 + '755', 'OB')},
                {'url': 'transmissionMethod', 'valueCodeableConcept': cc(X12 + '756', 'EL')},
                {'url': 'controlNumber', 'valueString': 'ATT-2026-0001'},
                {'url': 'description', 'valueString': 'Home sleep study summary'}]}],
            'sequence': 4, 'category': cc(PASTEMP, 'additionalInformation'),
            'valueReference': ctx.ref('DocumentReference', 'pas-doc-001')})
        si.append({'sequence': 5, 'category': cc(PASTEMP, 'freeFormMessage'),
                   'valueString': 'Synthetic PAS test request. Patient uses the device nightly.'})
    return si


def update_supporting_info():
    return [
        {'modifierExtension': [ext(PAS + 'modifierextension-infoCancelled', valueBoolean=True)],
         'sequence': 1, 'category': cc(PASTEMP, 'patientEvent'), 'timingPeriod': {'start': SERVICE_START, 'end': SERVICE_END}},
        {'extension': [ext(PAS + 'extension-infoChanged', valueCode='changed')],
         'sequence': 2, 'category': cc(PASTEMP, 'freeFormMessage'), 'valueString': 'Updated order: quantity changed and one item cancelled.'},
    ]


def claim(ctx, spec):
    kind = spec['kind']  # initial | update | inquiry
    rich = spec.get('rich', False)
    if kind == 'inquiry':
        profile = 'profile-claim-inquiry'
    elif kind == 'update' or not ctx.v221:
        profile = 'profile-claim-update'
    else:
        profile = 'profile-claim'
    c = {'resourceType': 'Claim', 'id': ctx.id('Claim', spec['lid']), 'meta': {'profile': [PAS + profile]}}
    e = []
    if rich:
        if ctx.v221:
            e.append({'url': PAS + 'extension-TransmissionIdentifiers', 'extension': [
                {'url': 'applicationSenderCode', 'valueString': 'LSMGAPP'},
                {'url': 'applicationReceiverCode', 'valueString': 'NSHPAPP'},
                {'url': 'interchangeSenderID', 'valueString': 'LSMG01'},
                {'url': 'interchangeReceiverID', 'valueString': 'NSHP01'}]})
        e.append(ext(PAS + 'extension-levelOfServiceCode', valueCodeableConcept=cc(X12 + '1338', 'E', 'Elective')))
        if kind != 'inquiry':
            e.append(ext(CLAIM_ENCOUNTER_EXT, valueReference=ctx.ref('Encounter', 'pas-enc-001')))
        e.append({'url': PAS + 'extension-conditionCode', 'extension': [
            {'url': 'category', 'valueCodeableConcept': cc(X12 + '1136', 'E1')},
            {'url': 'indicator', 'valueBoolean': True},
            {'url': 'code', 'valueCodeableConcept': cc(X12 + '1321', '38')}]})
        e.append({'url': PAS + 'extension-homeHealthCareInformation', 'extension': [
            {'url': 'prognosis', 'valueCodeableConcept': cc(X12 + '923', '5')},
            {'url': 'date', 'valueDate': '2026-08-30'}]})
        if ctx.v221:
            e.append(ext(PAS + 'extension-authorizationNumber', valueString=spec.get('claim_auth', f"AUTH-PRIOR-{spec['ident']}")))
            e.append(ext(PAS + 'extension-administrationReferenceNumber', valueString=f"ADMREF-PRIOR-{spec['ident']}"))
    if kind == 'update' or (kind == 'inquiry' and rich) or (kind == 'update' and ctx.v221):
        code = '3' if spec.get('cancel_all') else ('I' if kind == 'inquiry' else 'S')
        e.append(ext(PAS + 'extension-certificationType', valueCodeableConcept=cc(X12 + '1322', code)))
    if e:
        c['extension'] = e
    ident = {'system': SID + 'pa-request-id', 'value': spec['ident']}
    if rich:
        ident = {'extension': [ext(PAS + 'extension-identifierJurisdiction', valueCodeableConcept=cc('https://www.usps.com/', 'MO')),
                               ext(PAS + 'extension-identifierSubDepartment', valueString='DME-UM')], **ident}
    c['identifier'] = [ident]
    c['status'] = 'active'
    if rich and kind == 'inquiry':
        c['billablePeriod'] = {'start': SERVICE_START, 'end': SERVICE_END}
    c['type'] = cc('http://terminology.hl7.org/CodeSystem/claim-type', 'professional')
    c['use'] = 'preauthorization'
    c['patient'] = ctx.ref('Patient', 'pas-pat-001')
    c['created'] = spec.get('created', TIMESTAMP)
    c['insurer'] = ctx.ref('Organization', 'pas-org-payer')
    c['provider'] = ctx.ref('Organization', 'pas-org-provider')
    c['priority'] = cc('http://terminology.hl7.org/CodeSystem/processpriority', 'normal')
    if spec.get('related'):
        c['related'] = [{'claim': ctx.ref('Claim', spec['related']),
                         'relationship': cc('http://terminology.hl7.org/CodeSystem/ex-relatedclaimrelationship', 'prior')}]
    if rich:
        c['careTeam'] = [
            {'extension': [ext(PAS + 'extension-careTeamClaimScope', valueBoolean=True)], 'sequence': 1,
             'provider': ctx.ref('PractitionerRole', 'pas-role-001'), 'role': cc(X12 + '98', 'DK'), 'qualification': cc(NUCC, '207RP1001X')},
            {'extension': [ext(PAS + 'extension-careTeamClaimScope', valueBoolean=False)], 'sequence': 2,
             'provider': ctx.ref('PractitionerRole', 'pas-role-001'), 'role': cc(X12 + '98', 'SJ'), 'qualification': cc(NUCC, '207RP1001X')},
        ]
        si = supporting_info(ctx, kind, spec)
        c['supportingInfo'] = si
    elif spec.get('update_si'):
        c['supportingInfo'] = update_supporting_info()
    diag = {'sequence': 1, 'diagnosisCodeableConcept': {'coding': [{'system': ICD10, 'code': 'G47.33'}]},
            'type': [cc(PASTEMP, 'principal')]}
    if rich and kind != 'inquiry':
        diag = {'extension': [ext(PAS + 'extension-diagnosisRecordedDate', valueDate='2026-08-28')], **diag}
    c['diagnosis'] = [diag]
    c['insurance'] = [{'sequence': 1, 'focal': True, 'coverage': ctx.ref('Coverage', 'pas-cov-001')}]
    if rich:
        c['accident'] = {'date': '2026-08-20', 'type': cc(X12 + '1362', 'OA'),
                         'locationAddress': {'line': ['12 Synthetic Road'], 'city': 'Springfield', 'state': 'MO',
                                             'postalCode': '65803', 'country': 'US'}}
    c['item'] = [claim_item(ctx, it, kind, rich) for it in spec['items']]
    return c


def bundle(ctx, spec, bundle_profile):
    entries = []

    def add(resource):
        entries.append({'fullUrl': ctx.url(resource['resourceType'], lid_of(resource)), 'resource': resource})

    lids = {}

    def lid_of(resource):
        return lids[id(resource)]

    def reg(resource, lid):
        lids[id(resource)] = lid
        return resource

    add(reg(claim(ctx, spec), spec['lid']))
    for prior in spec.get('prior_claims', []):
        add(reg(claim(ctx, prior), prior['lid']))
    add(reg(provider_org(ctx), 'pas-org-provider'))
    add(reg(payer_org(ctx), 'pas-org-payer'))
    add(reg(coverage(ctx), 'pas-cov-001'))
    add(reg(patient(ctx), 'pas-pat-001'))
    if spec.get('rich'):
        add(reg(practitioner_role(ctx), 'pas-role-001'))
        add(reg(practitioner(ctx), 'pas-prac-001'))
        add(reg(location(ctx), 'pas-loc-001'))
        add(reg(endpoint(ctx), 'pas-endpoint-001'))
        if spec['kind'] != 'inquiry':
            add(reg(encounter(ctx), 'pas-enc-001'))
            add(reg(condition(ctx), 'pas-cond-001'))
            add(reg(document_reference(ctx), 'pas-doc-001'))
    elif any(it.get('sr') for it in spec['items']):
        add(reg(practitioner(ctx), 'pas-prac-001'))
    if spec['kind'] != 'inquiry':
        for it in spec['items']:
            if it.get('sr'):
                add(reg(service_request(ctx, it['sr'], it['code'], f"CRD-{spec['ident']}-{it['seq']}", it.get('code_end')), it['sr']))
    return {
        'resourceType': 'Bundle',
        'id': ctx.id('Bundle', spec['bundle_id']),
        'meta': {'profile': [PAS + bundle_profile]},
        'identifier': {'system': SID + 'submitter-transaction-id', 'value': spec['trn']},
        'type': 'collection',
        'timestamp': spec.get('timestamp', TIMESTAMP),
        'entry': entries,
    }


def submit_bundle(version, spec):
    ctx = Ctx(version, spec.get('scheme', 'abs'), spec['bundle_id'])
    return bundle(ctx, spec, 'profile-pas-request-bundle')


def inquire_bundle(version, spec):
    ctx = Ctx(version, spec.get('scheme', 'abs'), spec['bundle_id'])
    return bundle(ctx, spec, 'profile-pas-inquiry-request-bundle')


def subscription(version):
    sub = {
        'resourceType': 'Subscription',
        'meta': {'profile': [PAS + 'profile-subscription' if version == 'v221' else BACKPORT + 'backport-subscription']},
        'status': 'requested',
        'end': '2030-12-31T23:59:59Z',
        'reason': 'Synthetic PAS test: notify the requesting provider organization when a pended prior authorization is finalized',
        'criteria': TOPIC,
        '_criteria': {'extension': [{'url': BACKPORT + 'backport-filter-criteria', 'valueString': f'org-identifier={NPI_PROVIDER_ORG}'}]},
        'channel': {
            'extension': [{'url': BACKPORT + 'backport-timeout', 'valueUnsignedInt': 60},
                          {'url': BACKPORT + 'backport-max-count', 'valuePositiveInt': 20}],
            'type': 'rest-hook',
            'endpoint': 'https://inferno.example/replaced-by-inferno',
            'payload': 'application/fhir+json',
            '_payload': {'extension': [{'url': BACKPORT + 'backport-payload-content',
                                        'valueCode': 'full-resource' if version == 'v221' else 'id-only'}]},
        },
    }
    return sub


# ------------------------------------------------------------------------------------------------
# Scenarios
# ------------------------------------------------------------------------------------------------
def item(seq, code, **kw):
    return dict(seq=seq, code=code, **kw)


def scenarios(version):
    v = version
    out = {}
    out['approval_submit'] = dict(kind='initial', lid='pas-claim-0001', ident='PA-2026-0001', trn='TRN-2026-0001', bundle_id='pas-req-0001',
                                  items=[item(1, 'A7030', trace='TRC-0001-1')],
                                  expected={'claim': 'A1', 'items': {1: 'A1'}, 'use_case': 'approval'})
    out['denial_submit'] = dict(kind='initial', lid='pas-claim-0002', ident='PA-2026-0002', trn='TRN-2026-0002', bundle_id='pas-req-0002',
                                items=[item(1, 'A9270', trace='TRC-0002-1')],
                                expected={'claim': 'A3', 'items': {1: 'A3'}, 'use_case': 'denial'})
    out['pended_submit'] = dict(kind='initial', lid='pas-claim-0003', ident='PA-2026-0003', trn='TRN-2026-0003', bundle_id='pas-req-0003',
                                items=[item(1, 'E0470', served='period', trace='TRC-0003-1')],
                                expected={'claim': 'A4', 'items': {1: 'A4'}, 'use_case': 'pended', 'after_finalize': {1: 'A1'}})
    ms_a = dict(kind='initial', rich=True, lid='pas-claim-0004', ident='PA-2026-0004', trn='TRN-2026-0004', bundle_id='pas-req-0004',
                items=[item(1, 'E0601', served='period', sr='pas-sr-0004-1', trace='TRC-0004-1', auth='AUTH-PA-2026-0004-1',
                            admref='ADMREF-PA-2026-0004-1', qty=1, price=1200.0),
                       item(2, 'A7030', served='date', sr='pas-sr-0004-2', trace='TRC-0004-2', code_end='A7039', qty=2, price=150.0)],
                expected={'claim': 'A6', 'items': {1: 'A6', 2: 'A1'}, 'addItem': True})
    ms_b = dict(kind='initial', rich=True, scheme='urn', lid='pas-claim-0005', ident='PA-2026-0005', trn='TRN-2026-0005', bundle_id='pas-req-0005',
                items=[item(1, 'E1390', served='period', sr='pas-sr-0005-1', trace='TRC-0005-1', qty=1, price=850.0),
                       item(2, 'A9999', served='date', sr='pas-sr-0005-2', trace='TRC-0005-2', qty=1, price=40.0)],
                expected={'claim': 'A4', 'items': {1: 'A4', 2: 'A3'}, 'communicationRequest': True, 'task': True, 'error': True,
                          'request': 'data-absent-reason'})
    out['must_support_submit'] = [ms_a, ms_b]
    inq_1 = dict(kind='inquiry', rich=True, lid='pas-inq-0004', ident='PA-2026-0004', trn='TRN-2026-0104', bundle_id='pas-inq-0004',
                 items=[item(1, 'E0601', served='period', trace='TRC-0004-1', auth='AUTH-PA-2026-0004-1', admref='ADMREF-PA-2026-0004-1', last_decision='A6'),
                        item(2, 'A7030', served='date', trace='TRC-0004-2', code_end='A7039', last_decision='A1')],
                 expected={'items': {1: 'A6', 2: 'A1'}, 'replacedAuthorization': {'AUTH-PA-2026-0004-1': 'AUTH-PA-2026-0004-1-R1'}})
    inq_2 = dict(kind='inquiry', rich=True, scheme='urn', lid='pas-inq-0104', ident='PA-2026-0104', trn='TRN-2026-0105', bundle_id='pas-inq-0104',
                 items=[item(1, 'E1390', served='period', trace='TRC-0104-1', auth='AUTH-PA-2026-0104-1', admref='ADMREF-PA-2026-0104-1', last_decision='A4'),
                        item(2, 'A9999', served='date', trace='TRC-0104-2', last_decision='A3')],
                 expected={'items': {1: 'A4', 2: 'A3'}, 'request': 'data-absent-reason', 'task': True, 'error': True,
                           'replacedAuthorization': {'AUTH-PA-2026-0104-1': 'AUTH-PA-2026-0104-1-R1'}})
    out['must_support_inquire'] = [inq_1, inq_2]
    if v == 'v201':
        out['pended_inquire'] = dict(kind='inquiry', lid='pas-inq-0003', ident='PA-2026-0003', trn='TRN-2026-0103', bundle_id='pas-inq-0003',
                                     items=[item(1, 'E0470', served='period', trace='TRC-0003-1')],
                                     expected={'items': {1: 'A1'}, 'note': 'after the pended claim was finalized'})
    if v == 'v221':
        base_items = [item(1, 'A7030', trace='TRC-0006-1'), item(2, 'A7035', trace='TRC-0006-2')]
        r0 = dict(kind='initial', lid='pas-claim-0006-r0', ident='PA-2026-0006', items=base_items)
        r1 = dict(kind='update', lid='pas-claim-0006-r1', ident='PA-2026-0006', related='pas-claim-0006-r0',
                  items=base_items + [item(3, 'A7037', trace='TRC-0006-3', info_changed='added')])
        r2 = dict(kind='update', lid='pas-claim-0006-r2', ident='PA-2026-0006', related='pas-claim-0006-r1', update_si=True,
                  items=[item(1, 'A7030', trace='TRC-0006-1', qty=2, info_changed='changed'),
                         item(2, 'A7035', trace='TRC-0006-2', cancelled=True, info_changed='changed'),
                         item(3, 'A7037', trace='TRC-0006-3')])
        r3 = dict(kind='update', lid='pas-claim-0006-r3', ident='PA-2026-0006', related='pas-claim-0006-r2', cancel_all=True,
                  items=[item(1, 'A7030', trace='TRC-0006-1', qty=2, cancelled=True),
                         item(2, 'A7035', trace='TRC-0006-2', cancelled=True),
                         item(3, 'A7037', trace='TRC-0006-3', cancelled=True)])
        out['claim_update_initial_submit'] = dict(r0, trn='TRN-2026-0006', bundle_id='pas-req-0006-r0',
                                                  expected={'items': {1: 'A1', 2: 'A1'}})
        out['claim_update_add_item_submit'] = dict(r1, trn='TRN-2026-0007', bundle_id='pas-req-0006-r1', prior_claims=[r0],
                                                   expected={'items': {1: 'A1', 2: 'A1', 3: 'A1'}})
        out['claim_update_modify_cancel_submit'] = dict(r2, trn='TRN-2026-0008', bundle_id='pas-req-0006-r2',
                                                        prior_claims=[dict(r1, related=None)] if False else [r1],
                                                        expected={'items': {1: 'A1', 2: 'C', 3: 'A1'}})
        out['claim_update_cancel_all_submit'] = dict(r3, trn='TRN-2026-0009', bundle_id='pas-req-0006-r3', prior_claims=[r2],
                                                     expected={'claim': 'C', 'items': {1: 'C', 2: 'C', 3: 'C'}})
    return out


INPUT_MAP = {
    'v221': [
        ('server_endpoint', {'value': '${FHIR_BASE}'}),
        ('access_token', {'value': '${NOTIFICATION_TOKEN}'}),
        ('subscription_resource', {'file': 'subscription.json'}),
        ('approval_pa_submit_request_payload', {'file': 'approval_submit.json'}),
        ('denial_pa_submit_request_payload', {'file': 'denial_submit.json'}),
        ('pended_pa_submit_request_payload', {'file': 'pended_submit.json'}),
        ('claim_update_initial_submit_payload', {'file': 'claim_update_initial_submit.json'}),
        ('claim_update_add_item_submit_payload', {'file': 'claim_update_add_item_submit.json'}),
        ('claim_update_modify_cancel_submit_payload', {'file': 'claim_update_modify_cancel_submit.json'}),
        ('claim_update_cancel_all_submit_payload', {'file': 'claim_update_cancel_all_submit.json'}),
        ('must_support_pa_submit_request_payload', {'file': 'must_support_submit.json'}),
        ('must_support_pa_inquire_request_payload', {'file': 'must_support_inquire.json'}),
    ],
    'v201': [
        ('server_endpoint', {'value': '${FHIR_BASE}'}),
        ('access_token', {'value': '${NOTIFICATION_TOKEN}'}),
        ('subscription_resource', {'file': 'subscription.json'}),
        ('approval_pa_submit_request_payload', {'file': 'approval_submit.json'}),
        ('denial_pa_submit_request_payload', {'file': 'denial_submit.json'}),
        ('pended_pa_submit_request_payload', {'file': 'pended_submit.json'}),
        ('pended_pa_inquire_request_payload', {'file': 'pended_inquire.json'}),
        ('must_support_pa_submit_request_payload', {'file': 'must_support_submit.json'}),
        ('must_support_pa_inquire_request_payload', {'file': 'must_support_inquire.json'}),
    ],
}
SUITE_IDS = {'v221': 'davinci_pas_server_suite_v221', 'v201': 'davinci_pas_server_suite_v201'}


def write_json(path, data):
    with open(path, 'w') as f:
        json.dump(data, f, indent=2)
        f.write('\n')


def build(version):
    outdir = os.path.join(HERE, version)
    os.makedirs(outdir, exist_ok=True)
    expected = {}
    for name, spec in scenarios(version).items():
        is_inquiry = 'inquire' in name
        maker = inquire_bundle if is_inquiry else submit_bundle
        if isinstance(spec, list):
            data = [maker(version, s) for s in spec]
            expected[name] = [dict(s['expected'], claimIdentifier=s['ident'], bundle=s['bundle_id']) for s in spec]
        else:
            data = maker(version, spec)
            expected[name] = dict(spec['expected'], claimIdentifier=spec['ident'], bundle=spec['bundle_id'])
        write_json(os.path.join(outdir, name + '.json'), data)
    write_json(os.path.join(outdir, 'subscription.json'), subscription(version))
    inputs = {'suite_id': SUITE_IDS[version],
              'note': 'Synthetic data. Values ${FHIR_BASE} and ${NOTIFICATION_TOKEN} are substituted by harness/run_suite.py.',
              'inputs': [dict(name=n, **v) for n, v in INPUT_MAP[version]]}
    write_json(os.path.join(outdir, 'inputs.json'), inputs)
    write_json(os.path.join(outdir, 'expected.json'), expected)
    return outdir


if __name__ == '__main__':
    for v in ('v221', 'v201'):
        print('wrote', build(v))
