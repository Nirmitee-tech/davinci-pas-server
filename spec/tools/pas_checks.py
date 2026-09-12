#!/usr/bin/env python3
"""Offline PAS checks that mirror the non-validator logic of the Inferno Da Vinci PAS server tests.

Source of truth: davinci-pas-test-kit 0.15.2 (inferno_core 1.4.3, subscriptions_test_kit 0.12.2).
Mirrored logic (file names are relative to lib/davinci_pas_test_kit):
  - cross_suite/must_support/*.rb and inferno_core dsl/must_support_assessment.rb,
    dsl/fhir_resource_navigation.rb (must support element, slice and extension detection)
  - cross_suite/pas_bundle_validation.rb (bundle structure, referenced resources, echoed resources)
  - server/pas_claim_response_decision_test.rb (review action codes)
  - server/v2.2.1/claim_updates/server_claim_update_response_tests.rb (item sequence echo)
  - server/v2.2.1/claim_inquiry/server_claim_inquiry_response_tests.rb (request or DataAbsentReason)
  - server/v2.2.1/server_replaced_pa_inquire_reference_test.rb (replaced reference number)
  - server/v2.2.1/pas_server_no_custom_extensions_test.rb (no custom extensions)
  - cross_suite/pas_subscription_verification.rb (Subscription input rules)

It does NOT run the HL7 FHIR validator (profile conformance, terminology). Those checks only happen
inside Inferno. Python 3 stdlib only.

Library use: import pas_checks; see the functions at the bottom (check_* return a list of error strings).
CLI: python3 pas_checks.py ms --version v221 --submit-requests a.json b.json --submit-responses r.json ...
"""
import argparse
import copy
import json
import os
import re
import sys
from datetime import date

HERE = os.path.dirname(os.path.abspath(__file__))
META_DIR = os.path.normpath(os.path.join(HERE, '..', 'ms-metadata'))
DAR_URL = 'http://hl7.org/fhir/StructureDefinition/data-absent-reason'
PAS_SD = 'http://hl7.org/fhir/us/davinci-pas/StructureDefinition/'
REVIEW_ACTION_URL = PAS_SD + 'extension-reviewAction'
REVIEW_ACTION_CODE_URL = PAS_SD + 'extension-reviewActionCode'
REVIEW_ACTION_SYSTEM = 'https://codesystem.x12.org/005010/306'
X12_DECISIONS = {'approval': 'A1', 'denial': 'A3', 'pended': 'A4'}
REFERENCE_NUMBER_URLS = [PAS_SD + 'extension-authorizationNumber', PAS_SD + 'extension-administrationReferenceNumber']
PAS_TOPIC = 'http://hl7.org/fhir/us/davinci-pas/SubscriptionTopic/PASSubscriptionTopic'
FILTER_URL = 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-filter-criteria'
PAYLOAD_CONTENT_URL = 'http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/backport-payload-content'

# pas_server_no_custom_extensions_test.rb VALID_EXTENSION_URLS (verbatim list of allowed URLs)
_US_CORE = ['us-core-birthsex', 'us-core-direct', 'us-core-ethnicity', 'us-core-race', 'uscdi-requirement',
            'us-core-extension-questionnaire-uri', 'us-core-genderIdentity', 'us-core-jurisdiction', 'us-core-sex',
            'us-core-tribal-affiliation', 'us-core-medication-adherence']
_PAS = ['extension-TransmissionIdentifiers', 'extension-administrationReferenceNumber', 'extension-admissionDates',
        'extension-authorizationNumber', 'extension-authorizedProviderType', 'extension-careTeamClaimScope',
        'extension-certificationType', 'extension-claimResponseReviewer', 'extension-communicatedDiagnosis',
        'extension-conditionCode', 'extension-contentModifier', 'extension-diagnosisRecordedDate',
        'extension-dischargeDate', 'extension-documentInformation', 'extension-epsdtIndicator',
        'extension-errorElement', 'extension-errorFollowupAction', 'extension-errorPath',
        'extension-homeHealthCareInformation', 'extension-identifierJurisdiction', 'extension-identifierSubDepartment',
        'extension-infoChanged', 'extension-itemAuthorizedDetail', 'extension-itemAuthorizedProvider',
        'extension-itemCategory', 'extension-itemCertificationEffectiveDate', 'extension-itemCertificationExpirationDate',
        'extension-itemCertificationIssueDate', 'extension-itemPreAuthIssueDate', 'extension-itemPreAuthPeriod',
        'extension-itemRequestedServiceDate', 'extension-itemTraceNumber', 'extension-levelOfServiceCode',
        'extension-militaryStatus', 'extension-nursingHomeLevelOfCare', 'extension-nursingHomeResidentialStatus',
        'extension-patientStatus', 'extension-productOrServiceCodeEnd', 'extension-requestedService',
        'extension-revenueCode', 'extension-revenueUnitRateLimit', 'extension-reviewAction',
        'extension-reviewActionCode', 'extension-serviceItemRequestType', 'extension-serviceLineNumber',
        'extension-timingcalendarpattern', 'extension-timingdeliverypattern', 'modifierextension-infoCancelled']
VALID_EXTENSION_URLS = set(
    ['http://hl7.org/fhir/us/core/StructureDefinition/' + x for x in _US_CORE]
    + ['http://hl7.org/fhir/StructureDefinition/condition-assertedDate']
    + ['http://hl7.org/fhir/us/davinci-crd/StructureDefinition/' + x for x in
       ['ext-coverage-information', 'ext-billing-options', 'ext-request-category']]
    + ['http://hl7.org/fhir/StructureDefinition/codeOptions', 'http://hl7.org/fhir/StructureDefinition/alternate-reference',
       'http://hl7.org/fhir/5.0/StructureDefinition/extension-CommunicationRequest.payload.content',
       'http://hl7.org/fhir/5.0/StructureDefinition/extension-Task.requestedPeriod',
       'http://hl7.org/fhir/5.0/StructureDefinition/extension-Task.requestedPerformer',
       'http://hl7.org/fhir/5.0/StructureDefinition/extension-Task.performer',
       'http://hl7.org/fhir/StructureDefinition/request-doNotPerform',
       'http://hl7.org/fhir/5.0/StructureDefinition/extension-Task.input.value',
       'http://hl7.org/fhir/5.0/StructureDefinition/extension-Task.output.value',
       'http://hl7.org/fhir/5.0/StructureDefinition/extension-Task.statusReason',
       'http://hl7.org/fhir/us/davinci-hrex/StructureDefinition/extension-CoverageDavinciWellknownLocation']
    + [PAS_SD + x for x in _PAS]
    + ['http://hl7.org/fhir/5.0/StructureDefinition/extension-Claim.encounter', DAR_URL]
    + ['http://hl7.org/fhir/uv/subscriptions-backport/StructureDefinition/' + x for x in
       ['backport-filter-criteria', 'backport-heartbeat-period', 'backport-timeout', 'backport-max-count',
        'backport-channel-type', 'backport-payload-content', 'capabilitystatement-subscriptiontopic-canonical']]
    + ['http://hl7.org/fhir/us/davinci-dtr/StructureDefinition/' + x for x in
       ['activeRole', 'alternativeExpression', 'containedReference', 'information-origin', 'intendedUse', 'qr-context',
        'estimated-completion-time', 'questionnaireAudience', 'qr-coverage', 'request-specific']]
)
# SDC urls are also allowed by the kit; they are matched by prefix below.
SDC_PREFIX = 'http://hl7.org/fhir/uv/sdc/StructureDefinition/sdc-'


# ----------------------------------------------------------------------------------------------
# Ruby-like helpers
# ----------------------------------------------------------------------------------------------
class Prim:
    """A primitive value that carries a `_field` element (id/extension), like Inferno PrimitiveType."""

    def __init__(self, value, source):
        self.value = value
        self.source = source or {}

    @property
    def extension(self):
        return self.source.get('extension', [])


def present(v):
    if v is None or v is False:
        return False
    if isinstance(v, Prim):
        return present(v.value) or bool(v.extension)
    if isinstance(v, str):
        return v.strip() != ''
    if isinstance(v, (list, dict)):
        return len(v) > 0
    return True


def value_not_empty(v):
    return present(v) or v is False


def blank(v):
    return not present(v)


def wrap(v):
    if v is None:
        return []
    if isinstance(v, list):
        return v
    return [v]


def path_segments(path):
    segs, cur, depth, inq = [], '', 0, False
    for ch in path or '':
        if ch == "'":
            cur += ch
            inq = not inq
        elif ch == '(':
            cur += ch
            if not inq:
                depth += 1
        elif ch == ')':
            cur += ch
            if not inq:
                depth -= 1
        elif ch == '.':
            if depth == 0 and not inq:
                segs.append(cur)
                cur = ''
            else:
                cur += ch
        else:
            cur += ch
    if cur:
        segs.append(cur)
    return segs


_DATE_RE = re.compile(r'^\d{4}(-\d{2}(-\d{2})?)?')


def looks_like(value, code):
    """Approximates `element.is_a? FHIR.const_get(code)` for JSON values."""
    if code in ('Date', 'DateTime'):
        return isinstance(value, str) and bool(_DATE_RE.match(value))
    if code == 'String':
        return isinstance(value, str)
    if code == 'Integer':
        return isinstance(value, int) and not isinstance(value, bool)
    if isinstance(value, dict) and 'resourceType' in value:
        return value['resourceType'] == code
    if not isinstance(value, dict):
        return False
    keys = set(value.keys()) - {'id', 'extension'}
    shapes = {
        'Period': {'start', 'end'},
        'Reference': {'reference', 'identifier', 'display', 'type'},
        'CodeableConcept': {'coding', 'text'},
        'Coding': {'system', 'code', 'display', 'version', 'userSelected'},
        'Identifier': {'system', 'value', 'type', 'use', 'period', 'assigner'},
        'Quantity': {'value', 'unit', 'system', 'code', 'comparator'},
        'Address': {'line', 'city', 'state', 'postalCode', 'country', 'district', 'use', 'type', 'text', 'period'},
        'Timing': {'event', 'repeat', 'code'},
    }
    if code in shapes:
        return bool(keys) and keys <= shapes[code]
    return False


# ----------------------------------------------------------------------------------------------
# Navigation (inferno_core dsl/fhir_resource_navigation.rb)
# ----------------------------------------------------------------------------------------------
class Navigator:
    def __init__(self, must_supports=None):
        self.ms = must_supports or {}

    def field_value(self, element, name):
        if not isinstance(element, dict):
            return None
        value = element.get(name)
        source = element.get('_' + name)
        if present(source):
            return Prim(value, source)
        return value

    def get_next_value(self, element, prop):
        if isinstance(element, Prim) or not isinstance(element, dict):
            return None
        m = re.search(r"(?<=where\(url=').*(?='\))", prop)
        if m and m.group(0):
            return element if element.get('url') == m.group(0) else None
        if '[x]:' in prop:
            return self.field_value(element, prop.split(':', 1)[1])
        if prop.endswith('[x]'):
            prefix = prop[:-3]
            for key in element.keys():
                if key.startswith(prefix) and value_not_empty(self.field_value(element, key)):
                    return self.field_value(element, key)
            return None
        if ':' in prop and 'url' not in prop:
            return self.find_slice_via_discriminator(element, prop)
        return self.field_value(element, prop)

    def resolve_path(self, elements, path):
        elements = wrap(elements)
        if blank(path):
            return elements
        segs = path_segments(path)
        out = []
        for el in elements:
            child = self.get_next_value(el, segs[0])
            out.extend(self.resolve_path(child, '.'.join(segs[1:])))
        return [o for o in out if o is not None]

    def find_a_value_at(self, given, path, block=None, include_dar=False, recursive=()):
        if given is None:
            return None
        elements = wrap(given)
        if path == '' or path is None:
            return self.find_in_elements(elements, block, include_dar)
        segs = path_segments(path)
        seg = segs[0]
        rem = '.'.join(segs[1:])
        for el in elements:
            child = self.get_next_value(el, seg)
            found = self.find_a_value_at(child, rem, block, include_dar, recursive)
            if value_not_empty(found):
                return found
            if seg in recursive:
                nested = self.find_a_value_at(child, path, block, include_dar, recursive)
                if value_not_empty(nested):
                    return nested
        return None

    @staticmethod
    def has_dar(el):
        exts = el.extension if isinstance(el, Prim) else (el.get('extension') if isinstance(el, dict) else None)
        return any(isinstance(e, dict) and e.get('url') == DAR_URL for e in (exts or []))

    def find_in_elements(self, elements, block, include_dar):
        if not include_dar:
            elements = [e for e in elements if not self.has_dar(e)]
        if block:
            for e in elements:
                if block(e):
                    return e
            return None
        return elements[0] if elements else None

    def find_slice_via_discriminator(self, element, prop):
        name, slice_name = prop.split(':', 1)
        sl = next((s for s in self.ms.get('slices', []) if s.get('slice_name') == slice_name), None)
        if not sl:
            return None
        for candidate in wrap(element.get(name)):
            if self.matching_slice(candidate, sl['discriminator']):
                return candidate
        return None

    def matching_slice(self, sl, disc):
        t = disc.get('type')
        if not isinstance(sl, dict):
            return t == 'type' and looks_like(sl, disc.get('code'))
        if t == 'patternCodeableConcept':
            target = sl.get(disc['path']) if present(disc.get('path')) else sl
            codings = target.get('coding', []) if isinstance(target, dict) else []
            return any(c.get('code') == disc.get('code') and c.get('system') == disc.get('system') for c in codings)
        if t == 'patternCoding':
            target = sl.get(disc['path']) if present(disc.get('path')) else sl
            return isinstance(target, dict) and target.get('code') == disc.get('code') and target.get('system') == disc.get('system')
        if t == 'patternIdentifier':
            return sl.get('system') == disc.get('system')
        if t == 'value':
            values = [dict(v, path=path_segments(v['path'])) for v in disc['values']]
            return self.verify_slice_by_values(sl, values)
        if t == 'type':
            resolved = self.resolve_path(sl, disc.get('path'))
            return bool(resolved) and looks_like(resolved[0], disc.get('code'))
        if t == 'requiredBinding':
            if present(disc.get('path')):
                codings = [c for v in self.resolve_path(sl, disc['path']) for c in wrap(v.get('coding') if isinstance(v, dict) else None)]
            elif 'code' in sl and 'coding' not in sl:
                codings = [sl]
            else:
                codings = wrap(sl.get('coding'))
            return any(required_binding_match(c, disc.get('values', [])) for c in codings)
        return False

    def verify_slice_by_values(self, element, value_defs):
        value_defs = copy.deepcopy(value_defs)
        prefixes = []
        for vd in value_defs:
            p = vd['path'][0] if vd['path'] else None
            if p not in prefixes:
                prefixes.append(p)
        for prefix in prefixes:
            if prefix is None:
                return False
            for_path = [vd for vd in value_defs if (vd['path'][0] if vd['path'] else None) == prefix]
            for vd in for_path:
                vd['path'] = vd['path'][1:]

            def block(el_found, defs=for_path):
                return self.current_and_child_values_match(el_found, defs)

            if not value_not_empty(self.find_a_value_at(element, prefix, block)):
                return False
        return True

    def current_and_child_values_match(self, el_found, defs):
        child = [d for d in defs if d['path']]
        current = [d for d in defs if not d['path']]
        value = el_found.value if isinstance(el_found, Prim) else el_found
        cur_ok = all(d['value'] == value and type(d['value']) is type(value) for d in current)
        child_ok = self.verify_slice_by_values(el_found, child) if child else True
        return cur_ok and child_ok


def required_binding_match(coding, values):
    for v in values:
        if isinstance(v, str) and v == coding.get('code'):
            return True
        if isinstance(v, dict) and v.get('system') == coding.get('system') and v.get('code') == coding.get('code'):
            return True
    return False


# ----------------------------------------------------------------------------------------------
# Must support assessment (inferno_core dsl/must_support_assessment.rb)
# ----------------------------------------------------------------------------------------------
class MustSupport:
    def __init__(self, profile_meta):
        self.ms = profile_meta['must_supports']
        self.nav = Navigator(self.ms)
        self.recursive = tuple(wrap(self.ms.get('recursive_elements')))

    @property
    def extensions(self):
        return self.ms.get('extensions', [])

    def missing(self, resources):
        if not resources:
            return None
        self.missing_elements = [e for e in self.ms.get('elements', []) if not any(self.populates(r, e) for r in resources)]
        self.missing_slices = [s for s in self.ms.get('slices', []) if not any(self.find_slice(r, s) for r in resources)]
        self.missing_exts = [x for x in self.extensions if not any(self.has_extension(r, x) for r in resources)]
        if self.ms.get('choices'):
            self.handle_choices()
        out = []
        for e in self.missing_elements:
            fixed = e.get('fixed_value')
            out.append(f"{e['path']}:{str(fixed).lower() if isinstance(fixed, bool) else fixed}" if present(fixed) else e['path'])
        out += [s['slice_id'] for s in self.missing_slices]
        out += [x['id'] for x in self.missing_exts]
        return out

    def handle_choices(self):
        choices = self.ms.get('choices', [])

        def supported(choice):
            if not choice:
                return False
            if choice.get('paths') and any(all(e['path'] != p for e in self.missing_elements) for p in choice['paths']):
                return True
            if choice.get('extension_ids') and any(all(x['id'] != i for x in self.missing_exts) for i in choice['extension_ids']):
                return True
            if choice.get('slice_names') and any(all(s.get('name') != n for s in self.missing_slices) for n in choice['slice_names']):
                return True
            if choice.get('elements') and any(all(not (e['path'] == c['path'] and e.get('fixed_value') == c.get('fixed_value')) for e in self.missing_elements) for c in choice['elements']):
                return True
            return False

        self.missing_elements = [e for e in self.missing_elements if not supported(next((c for c in choices if e['path'] in wrap(c.get('paths')) or any(x['path'] == e['path'] for x in wrap(c.get('elements')))), None))]
        self.missing_exts = [x for x in self.missing_exts if not supported(next((c for c in choices if x['id'] in wrap(c.get('extension_ids'))), None))]
        self.missing_slices = [s for s in self.missing_slices if not supported(next((c for c in choices if s.get('name') in wrap(c.get('slice_names'))), None))]

    def has_extension(self, resource, ext_def):
        url = ext_def['url'].split('|')[0]
        if ext_def['path'] == 'extension':
            return any(isinstance(e, dict) and (e.get('url') or '').split('|')[0] == url for e in wrap(resource.get('extension')))
        found = self.nav.find_a_value_at(resource, ext_def['path'], lambda el: isinstance(el, dict) and (el.get('url') or '').split('|')[0] == url, recursive=self.recursive)
        return present(found)

    def nav_compatible(self, path):
        logical, out = [], []
        for seg in path_segments(path):
            m = re.match(r'^(modifierExtension|extension):(.+)$', seg)
            if m:
                ext_type, ext_name = m.group(1), m.group(2)
                ext_path = '.'.join([p for p in ['.'.join(logical), ext_type] if p])
                d = self.ext_definition(ext_path, ext_type, ext_name)
                out.append(f"{ext_type}.where(url='{d['url'].split('|')[0]}')" if d else seg)
            else:
                out.append(seg)
            logical.append(seg.split(':')[0])
        return '.'.join(out)

    def ext_definition(self, ext_path, ext_type, ext_name):
        suffix = f'{ext_type}:{ext_name}'
        matching = [d for d in self.extensions if d['path'] == ext_path]
        for defs, how in ((matching, 'end'), (matching, 'in'), (self.extensions, 'end')):
            for d in defs:
                if (how == 'end' and d['id'].endswith(suffix)) or (how == 'in' and suffix in d['id']):
                    return d
        return None

    def populates(self, resource, ed):
        raw = ed['path']
        path = self.nav_compatible(raw)
        ms_urls = [x['url'].split('|')[0] for x in self.extensions if x['path'] == raw + '.extension']
        fixed = ed.get('fixed_value')

        def block(value):
            exts = value.extension if isinstance(value, Prim) else (value.get('extension') if isinstance(value, dict) else None)
            has_ms = any(isinstance(e, dict) and (e.get('url') or '').split('|')[0] in ms_urls for e in wrap(exts))
            v = value.value if isinstance(value, Prim) else value
            if isinstance(v, dict):
                without = {k: x for k, x in v.items() if k != 'extension'}
                ok = present(without)
            else:
                ok = present(v) or v is False
            if not (has_ms or ok):
                return False
            return blank(fixed) or v == fixed

        found = self.nav.find_a_value_at(resource, path, block, recursive=self.recursive)
        return present(found) or found is False

    def find_slice(self, resource, sl):
        disc = sl['discriminator']
        nav = self.nav

        def block(element):
            t = disc.get('type')
            if t == 'patternCodeableConcept':
                cp = f"{disc['path']}.coding" if present(disc.get('path')) else 'coding'
                return present(nav.find_a_value_at(element, cp, lambda c: isinstance(c, dict) and c.get('code') == disc.get('code') and c.get('system') == disc.get('system')))
            if t == 'patternCoding':
                cp = disc['path'] if present(disc.get('path')) else ''
                return present(nav.find_a_value_at(element, cp, lambda c: isinstance(c, dict) and c.get('code') == disc.get('code') and c.get('system') == disc.get('system')))
            if t == 'patternIdentifier':
                return present(nav.find_a_value_at(element, disc.get('path') or '', lambda i: isinstance(i, dict) and i.get('system') == disc.get('system')))
            if t == 'value':
                values = [dict(v, path=path_segments(v['path'])) for v in disc['values']]
                return any(nav.verify_slice_by_values(el, values) for el in wrap(element))
            if t == 'type':
                target = element.get('resource') if isinstance(element, dict) and 'resource' in element and disc.get('code') not in ('Period',) else element
                return looks_like(target, disc.get('code'))
            if t == 'requiredBinding':
                if isinstance(element, dict) and 'code' in element and 'coding' not in element and required_binding_match(element, disc.get('values', [])):
                    return True
                cp = f"{disc['path']}.coding" if present(disc.get('path')) else 'coding'
                return present(nav.find_a_value_at(element, cp, lambda c: isinstance(c, dict) and required_binding_match(c, disc.get('values', []))))
            return False

        return value_not_empty(nav.find_a_value_at(resource, sl['path'], block, recursive=self.recursive))


def remove_false_positives(missing, resources, resource_type):
    """cross_suite/must_support/must_support_data_gathering.rb remove_must_support_false_positives"""
    if not missing:
        return missing
    if resource_type == 'Coverage' and 'Coverage.relationship.coding:X12Code' in missing:
        if any('x12.org' in (c.get('system') or '') for r in resources for c in wrap((r.get('relationship') or {}).get('coding'))):
            missing = [m for m in missing if m not in ('Coverage.relationship.coding:X12Code', 'relationship.coding:X12Code.code')]
    if resource_type == 'ClaimResponse' and 'request' in missing:
        if any(isinstance(r.get('request'), dict) and any(e.get('url') == DAR_URL for e in wrap(r['request'].get('extension'))) for r in resources):
            missing = [m for m in missing if m != 'request']
    return missing


# ----------------------------------------------------------------------------------------------
# Must support test groups (server/generated/<version>/pas_server_must_support_group.rb)
# ----------------------------------------------------------------------------------------------
MS_TESTS = {
    'submit_request': [('pas_request_bundle', 'Bundle'), ('claim_update', 'Claim'), ('coverage', 'Coverage'),
                       ('encounter', 'Encounter'), ('insurer', 'Organization'), ('requestor', 'Organization'),
                       ('beneficiary', 'Patient'), ('subscriber', 'Patient'), ('practitioner', 'Practitioner'),
                       ('practitioner_role', 'PractitionerRole')],
    'submit_response': [('pas_response_bundle', 'Bundle'), ('claimresponse', 'ClaimResponse'),
                        ('communication_request', 'CommunicationRequest'), ('insurer', 'Organization'),
                        ('requestor', 'Organization'), ('beneficiary', 'Patient'), ('practitioner', 'Practitioner'),
                        ('practitioner_role', 'PractitionerRole'), ('task', 'Task')],
    'inquire_request': [('pas_inquiry_request_bundle', 'Bundle'), ('claim_inquiry', 'Claim'), ('coverage', 'Coverage'),
                        ('insurer', 'Organization'), ('requestor', 'Organization'), ('beneficiary', 'Patient'),
                        ('subscriber', 'Patient'), ('practitioner', 'Practitioner'), ('practitioner_role', 'PractitionerRole')],
    'inquire_response': [('pas_inquiry_response_bundle', 'Bundle'), ('claiminquiryresponse', 'ClaimResponse'),
                         ('insurer', 'Organization'), ('requestor', 'Organization'), ('beneficiary', 'Patient'),
                         ('practitioner', 'Practitioner'), ('practitioner_role', 'PractitionerRole'), ('task', 'Task')],
}
REQUEST_TYPE_KEYS = ['device_request', 'medication_request', 'nutrition_order', 'service_request']


def load_meta(version):
    with open(os.path.join(META_DIR, f'{version}.json')) as f:
        return json.load(f)


def bundles_from_body(body):
    """Accepts a Bundle, a Parameters (return), or a list of those."""
    out = []
    for item in wrap(body):
        if not isinstance(item, dict):
            continue
        if item.get('resourceType') == 'Parameters':
            out += [p['resource'] for p in item.get('parameter', []) if p.get('name') == 'return' and isinstance(p.get('resource'), dict) and p['resource'].get('resourceType') == 'Bundle']
        elif item.get('resourceType') == 'Bundle':
            out.append(item)
    return out


def gather(bodies):
    resources = []
    for body in bodies:
        for b in bundles_from_body(body):
            resources.append(b)
            resources += [e.get('resource') for e in b.get('entry', []) if e.get('resource')]
    return resources


def check_must_support(version, kind, bodies):
    """Returns a list of 'test: missing ...' strings. kind in MS_TESTS keys; bodies = request or response bodies."""
    meta = load_meta(version)
    resources = gather(bodies)
    errors = []
    for key, rtype in MS_TESTS[kind]:
        of_type = [r for r in resources if r.get('resourceType') == rtype]
        if not of_type:
            errors.append(f'{kind}/{key}: No {rtype} resources were found')
            continue
        missing = MustSupport(meta[key]).missing(of_type)
        missing = remove_false_positives(missing, of_type, rtype)
        if missing:
            errors.append(f'{kind}/{key}: Could not find {", ".join(missing)} in the {len(of_type)} provided {rtype} resource(s).')
    if kind == 'submit_request':
        types = {k: ''.join(w.capitalize() for w in k.split('_')) for k in REQUEST_TYPE_KEYS}
        seen = False
        for key, rtype in types.items():
            of_type = [r for r in resources if r.get('resourceType') == rtype]
            if not of_type:
                continue
            seen = True
            missing = MustSupport(meta[key]).missing(of_type)
            if missing:
                errors.append(f'{kind}/request_profiles/{key}: Could not find {", ".join(missing)} in the {len(of_type)} provided {rtype} resource(s).')
        if not seen:
            errors.append(f'{kind}/request_profiles: no DeviceRequest, MedicationRequest, NutritionOrder or ServiceRequest found')
    return errors


# ----------------------------------------------------------------------------------------------
# Bundle structure (cross_suite/pas_bundle_validation.rb, non-validator part)
# ----------------------------------------------------------------------------------------------
_URL_RE = re.compile(r'^https?://[^\s/$.?#][^\s]*$', re.I)
_URN_UUID_RE = re.compile(r'^urn:uuid:[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$', re.I)
_ABS_RE = re.compile(r'^[a-zA-Z][a-zA-Z0-9+.-]*:')


def extract_base_url(url):
    if blank(url):
        return ''
    m = re.match(r'^([a-zA-Z][a-zA-Z0-9+.-]*)://([^/:?#]+)(?::\d+)?([^?#]*)', url)
    if not m:
        return ''
    segs = m.group(3).split('/')
    return f'{m.group(1)}://{m.group(2)}' + '/'.join(segs[:-2])


def absolute_url(reference, base):
    if blank(reference):
        return None
    if blank(base) or reference.startswith('urn:uuid:') or _ABS_RE.match(reference):
        return reference
    return f'{base}/{reference}'


def _walk_references(resource, base, entries, errors, skip_claim_related=False, depth=0, seen_path=()):
    if depth > 40:
        errors.append('Reference traversal exceeded depth 40 (circular references?)')
        return
    if isinstance(resource, list):
        for x in resource:
            _walk_references(x, base, entries, errors, skip_claim_related, depth + 1, seen_path)
        return
    if not isinstance(resource, dict):
        return
    if isinstance(resource.get('reference'), str) and present(resource.get('reference')):
        ref = absolute_url(resource['reference'], base)
        matches = [e for e in entries if e.get('fullUrl') == ref]
        if len(matches) != 1:
            errors.append(f'The referenced {ref} resource SHALL appear exactly once in the Bundle, but found {len(matches)}.')
        if matches:
            _walk_references(matches[0].get('resource'), base, entries, errors, True, depth + 1, seen_path)
        return
    rtype = resource.get('resourceType')
    for key, value in resource.items():
        if key.startswith('_') or key in ('resourceType',):
            continue
        if key == 'request' and rtype == 'ClaimResponse':
            continue
        if key == 'related' and rtype == 'Claim' and skip_claim_related:
            continue
        if isinstance(value, (dict, list)):
            _walk_references(value, base, entries, errors, skip_claim_related, depth + 1, seen_path)


def check_request_structure(bundle, operation):
    errors = []
    entries = bundle.get('entry', [])
    first = entries[0].get('resource') if entries else None
    base = extract_base_url(entries[0].get('fullUrl') if entries else None)
    _walk_references(first, base, entries, errors)
    bid = bundle.get('id')
    if operation == 'submit':
        if not first or first.get('resourceType') != 'Claim':
            errors.append(f'[Bundle/{bid}]: The first bundle entry must be a Claim')
        seqs = [s.get('sequence') for s in (first or {}).get('supportingInfo', [])]
        if len(seqs) != len(set(seqs)):
            errors.append(f'[Claim/{(first or {}).get("id")}]: supportingInfo sequence values SHALL be unique')
        for e in entries:
            fu = e.get('fullUrl') or ''
            if not (_URL_RE.match(fu) or _URN_UUID_RE.match(fu)):
                errors.append(f'[Bundle/{bid}]: Bundle.entry.fullUrl {fu!r} SHALL be a valid url or urn:uuid')
    else:
        claim = next((e['resource'] for e in entries if e.get('resource', {}).get('resourceType') == 'Claim'), None)
        if not claim:
            errors.append(f'[Bundle/{bid}]: Claim must be present for inquiry request')
        for field, label in (('patient', 'a patient'), ('provider', 'a requesting provider organization'), ('insurer', 'a payer organization')):
            if blank(((claim or {}).get(field) or {}).get('reference')):
                errors.append(f'[Bundle/{bid}]: The Claim for inquiry operation must reference {label}.')
    return errors


def _identifier_eq(a, b):
    return json.dumps(a, sort_keys=True) == json.dumps(b, sort_keys=True)


def check_response_structure(response_bundle, request_bundle, operation):
    errors = []
    entries = response_bundle.get('entry', [])
    if not entries:
        return ['Response Bundle has no entries']
    first = entries[0].get('resource') or {}
    if first.get('resourceType') != 'ClaimResponse':
        errors.append(f'[Bundle/{response_bundle.get("id")}]: The first bundle entry must be a ClaimResponse')
    if operation == 'submit':
        base = extract_base_url(entries[-1].get('fullUrl'))
        _walk_references(first, base, entries, errors)
        if request_bundle:
            for re_ in entries:
                rr = re_.get('resource') or {}
                for q in request_bundle.get('entry', []):
                    qr = q.get('resource') or {}
                    if qr.get('resourceType') != rr.get('resourceType'):
                        continue
                    echoed = (present(q.get('fullUrl')) and q.get('fullUrl') == re_.get('fullUrl')) or \
                             (present(qr.get('id')) and qr.get('id') == rr.get('id')) or \
                             (present(qr.get('identifier')) and _identifier_eq(qr.get('identifier'), rr.get('identifier')))
                    if not echoed:
                        continue
                    if not (q.get('fullUrl') == re_.get('fullUrl') and qr.get('id') == rr.get('id') and _identifier_eq(qr.get('identifier'), rr.get('identifier'))):
                        errors.append(f'Resource {rr.get("resourceType")}/{rr.get("id")} is an entry in both the PA Request Bundle and the Response Bundle, but they do not have the same fullUrl or identifiers')
                    break
    return errors


# ----------------------------------------------------------------------------------------------
# Workflow specific checks
# ----------------------------------------------------------------------------------------------
def decision_codes(bundle):
    codes = []
    for e in bundle.get('entry', []):
        r = e.get('resource') or {}
        if r.get('resourceType') != 'ClaimResponse':
            continue
        for item in r.get('item', []):
            for adj in item.get('adjudication', []):
                for ext in adj.get('extension', []):
                    if ext.get('url') != REVIEW_ACTION_URL:
                        continue
                    for sub in ext.get('extension', []):
                        if sub.get('url') != REVIEW_ACTION_CODE_URL:
                            continue
                        for c in (sub.get('valueCodeableConcept') or {}).get('coding', []):
                            if c.get('system') == REVIEW_ACTION_SYSTEM:
                                codes.append(c.get('code'))
    return codes


def check_decision(bundle, use_case):
    target = X12_DECISIONS[use_case]
    return [] if target in decision_codes(bundle) else [f'Response Bundle did not have the expected adjudication code {target}.']


def check_claim_update_sequences(request_bundle, response_bundle, label):
    def first_of(bundle, rtype):
        entries = bundle.get('entry', [])
        if entries and (entries[0].get('resource') or {}).get('resourceType') == rtype:
            return entries[0]['resource']
        return next((e['resource'] for e in entries if (e.get('resource') or {}).get('resourceType') == rtype), None)

    claim = first_of(request_bundle, 'Claim') or {}
    cr = first_of(response_bundle, 'ClaimResponse') or {}
    submitted = [i.get('sequence') for i in claim.get('item', []) if i.get('sequence') is not None]
    returned = [i.get('itemSequence') for i in cr.get('item', []) if i.get('itemSequence') is not None]
    if not submitted:
        return []
    errors = []
    unexpected = [s for s in returned if s not in submitted]
    missing = [s for s in submitted if s not in returned]
    if unexpected:
        errors.append(f'{label}: ClaimResponse returned itemSequence {unexpected} not present on the submitted Claim (spec-34)')
    if missing:
        errors.append(f'{label}: ClaimResponse missing itemSequence {missing} (spec-46)')
    return errors


def check_inquiry_claim_reference(bodies):
    errors = []
    for b in bundles_from_body(bodies):
        for e in b.get('entry', []):
            r = e.get('resource') or {}
            if r.get('resourceType') != 'ClaimResponse':
                continue
            req = r.get('request') or {}
            has_ref = present(req.get('reference'))
            has_dar = any(x.get('url') == DAR_URL and present(x.get('valueCode')) for x in req.get('extension', []))
            if not (has_ref or has_dar):
                errors.append(f'ClaimResponse/{r.get("id")} has neither request.reference nor a DataAbsentReason extension (spec-44)')
    return errors


def reference_numbers(bundles):
    refs = {u: [] for u in REFERENCE_NUMBER_URLS}
    for b in bundles:
        for e in b.get('entry', []):
            r = e.get('resource') or {}
            for item in r.get('item', []) if isinstance(r.get('item'), list) else []:
                for x in item.get('extension', []):
                    if x.get('url') in refs and present(x.get('valueString')):
                        refs[x['url']].append(x['valueString'])
    return refs


def check_replaced_reference(request_bodies, response_bodies):
    """request_bodies[i] pairs with response_bodies[i]; passes if any pair returns a different number."""
    requests = [b for b in wrap(request_bodies)]
    if not any(any(reference_numbers([b]).values()) for b in bundles_from_body(requests)):
        return ['The inquiry requests did not contain an authorization number or administration reference number.']
    for req, resp in zip(requests, wrap(response_bodies)):
        sub = reference_numbers(bundles_from_body(req))
        ret = reference_numbers(bundles_from_body(resp))
        for url in REFERENCE_NUMBER_URLS:
            if sub[url] and ret[url] and any(v not in sub[url] for v in ret[url]):
                return []
    return ['Expected an inquiry response to contain a different authorization number or administration reference number (spec-47).']


def no_custom_extensions(bundle):
    custom = []

    def walk(value, path):
        if isinstance(value, dict):
            if 'url' in value and path and re.search(r'(extension|modifierExtension)\[\d+\]$', path):
                if path.count('extension') <= 1:
                    url = value.get('url')
                    if url not in VALID_EXTENSION_URLS and not (url or '').startswith(SDC_PREFIX):
                        custom.append(url)
            for k, v in value.items():
                walk(v, f'{path}.{k}' if path else k)
        elif isinstance(value, list):
            for i, v in enumerate(value):
                walk(v, f'{path}[{i}]')

    walk(bundle, '')
    return custom


def check_subscription(subscription, version):
    errors = []
    if version == 'v201':
        if subscription.get('criteria') != PAS_TOPIC:
            errors.append('criteria must be the PAS topic')
        filters = [x for x in ((subscription.get('_criteria') or {}).get('extension') or []) if x.get('url') == FILTER_URL]
        if len(filters) != 1:
            errors.append('exactly one backport-filter-criteria extension required')
        expected = 'id-only'
    else:
        if subscription.get('criteria') != PAS_TOPIC:
            errors.append('criteria must be the PAS topic (profile pattern)')
        for x in ((subscription.get('_criteria') or {}).get('extension') or []):
            if x.get('url') == FILTER_URL and not re.match(r'^org-identifier=\S+$', x.get('valueString') or ''):
                errors.append(f"filter criteria {x.get('valueString')!r} must match org-identifier=<identifier>")
        expected = 'full-resource'
    channel = subscription.get('channel') or {}
    if channel.get('type') != 'rest-hook':
        errors.append('channel.type must be rest-hook')
    content = next((x.get('valueCode') for x in ((channel.get('_payload') or {}).get('extension') or []) if x.get('url') == PAYLOAD_CONTENT_URL), None)
    if content != expected:
        errors.append(f'payload content must be {expected}, found {content}')
    if channel.get('header'):
        errors.append('channel.header must be omitted: Inferno appends "Authorization: Bearer <access_token>" itself')
    return errors


# ----------------------------------------------------------------------------------------------
# CLI
# ----------------------------------------------------------------------------------------------
def _load(path):
    with open(path) as f:
        return json.load(f)


def main(argv=None):
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest='cmd', required=True)
    ms = sub.add_parser('ms', help='must support coverage over request or response files')
    ms.add_argument('--version', choices=['v221', 'v201'], required=True)
    for kind in MS_TESTS:
        ms.add_argument('--' + kind.replace('_', '-') + 's', nargs='*', default=[], dest=kind)
    args = ap.parse_args(argv)
    failures = 0
    if args.cmd == 'ms':
        for kind in MS_TESTS:
            files = getattr(args, kind)
            if not files:
                continue
            errs = check_must_support(args.version, kind, [_load(p) for p in files])
            print(f'[{kind}] {"OK" if not errs else "MISSING"}')
            for e in errs:
                print('   ' + e)
            failures += len(errs)
    return 1 if failures else 0


if __name__ == '__main__':
    sys.exit(main())
