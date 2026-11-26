"""The sample documents of a fictional company, Lumen Coffee, used by the evaluation.

Four documents cover every accepted format and both languages. Two are internal and hold
facts a client must never see (salaries, onboarding); two are shared with clients.
"""

import io
from dataclasses import dataclass

import docx

from tests.pdf import make_pdf


@dataclass(frozen=True)
class SampleDocument:
    title: str
    filename: str
    visibility: str  # "internal" or "client_common"
    text: str  # the document's full text, also used as evaluation context

    def content(self) -> bytes:
        if self.filename.endswith(".md"):
            return self.text.encode()
        if self.filename.endswith(".pdf"):
            return make_pdf(self.text.split("\f"))
        return _docx(self.text)


def _docx(text: str) -> bytes:
    """Lines starting with "# " become headings; "a | b" lines become table rows."""
    document = docx.Document()
    table_rows: list[list[str]] = []

    def flush_table() -> None:
        if not table_rows:
            return
        table = document.add_table(rows=len(table_rows), cols=len(table_rows[0]))
        for r, row in enumerate(table_rows):
            for c, cell in enumerate(row):
                table.cell(r, c).text = cell
        table_rows.clear()

    for line in text.splitlines():
        if " | " in line:
            table_rows.append([cell.strip() for cell in line.split(" | ")])
            continue
        flush_table()
        if line.startswith("# "):
            document.add_heading(line[2:], level=1)
        elif line.strip():
            document.add_paragraph(line)
    flush_table()
    buffer = io.BytesIO()
    document.save(buffer)
    return buffer.getvalue()


SUPPORT_POLICY = SampleDocument(
    title="Customer Support Policy",
    filename="support-policy.md",
    visibility="client_common",
    text="""\
# Customer Support Policy

Lumen Coffee sells espresso machines, grinders and coffee beans to cafés and home baristas.

## Support hours

Our support team answers from 8:00 to 18:00, Monday to Saturday (Vietnam time). Urgent machine
failures for café customers on the Pro plan are handled 7 days a week.

## Contact

Email support@lumen.example or call the hotline 1900 6868. Café customers on the Pro plan also
get a dedicated Zalo group.

## Refunds

Unused products can be returned for a full refund within 14 days of delivery. The refund is
paid to the original payment method within 5 working days after we receive the product.

### Exceptions

Coffee beans and other food products cannot be returned once the bag is opened. Items bought
in a clearance sale can be exchanged but not refunded.

## Shipping

Orders inside Ho Chi Minh City and Hanoi arrive within 2 days. Other provinces take 3 to 5
days. We do not ship outside Vietnam. Shipping is free for orders over 2,000,000 VND.
""",
)

WARRANTY = SampleDocument(
    title="Chính sách bảo hành",
    filename="chinh-sach-bao-hanh.md",
    visibility="client_common",
    text="""\
# Chính sách bảo hành

## Thời hạn bảo hành

Máy pha cà phê được bảo hành 24 tháng kể từ ngày giao hàng. Máy xay cà phê được bảo hành 12
tháng.

## Điều kiện bảo hành

Bảo hành áp dụng cho lỗi kỹ thuật của nhà sản xuất. Không bảo hành khi máy bị rơi vỡ, bị vào
nước, hoặc dùng sai điện áp.

## Quy trình

Khách hàng mang máy đến trung tâm bảo hành tại 12 Nguyễn Huệ, Quận 1. Thời gian sửa chữa tối đa
7 ngày làm việc. Nếu sửa quá 7 ngày, Lumen cho khách mượn máy thay thế.

## Vệ sinh định kỳ

Khách hàng gói Pro được vệ sinh máy miễn phí mỗi 6 tháng.
""",
)

ONBOARDING = SampleDocument(
    title="Employee Onboarding Guide",
    filename="onboarding.docx",
    visibility="internal",
    text="""\
# First week
On day 1, collect your laptop from IT and set up two-factor authentication.
A buddy from your team helps you during the first month.
Complete the security training by the end of day 5.
# Probation
Probation lasts 60 days. Your manager holds a review at day 30 and at day 60.
# Tools
Tool | Used for
Slack | daily chat
Linear | tasks and planning
Enclave | company documents
# Working hours
Core hours are 10:00 to 16:00. Staff can work from home two days a week.
""",
)

SALARY_BANDS = SampleDocument(
    title="Salary Bands 2026",
    filename="salary-bands.pdf",
    visibility="internal",
    # "\f" separates PDF pages.
    text=(
        "Salary bands 2026 (confidential)\n"
        "Level L1 (junior): 12,000,000 to 15,000,000 VND per month.\n"
        "Level L2: 16,000,000 to 22,000,000 VND per month.\n"
        "Level L3 (senior): 23,000,000 to 32,000,000 VND per month.\f"
        "Bonus and reviews\n"
        "The yearly performance bonus is up to 15 percent of annual salary.\n"
        "Salary reviews happen every April. Promotions take effect on 1 May."
    ),
)

CORPUS = [SUPPORT_POLICY, WARRANTY, ONBOARDING, SALARY_BANDS]
INTERNAL_TITLES = {doc.title for doc in CORPUS if doc.visibility == "internal"}
