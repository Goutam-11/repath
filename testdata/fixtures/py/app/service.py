from .decorators import logged, route
from .models import Base, User
from .util import clamp


@logged
class Service(Base):
    def __init__(self, amount: int):
        self.amount = amount

    @route("/total")
    def total(self) -> int:
        return clamp(self.amount, 0, 100)

    @property
    def label(self) -> str:
        if self.total() > 0:
            return "ok"
        return "no"


def build(user: User) -> int:
    service = Service(0)
    return service.total()
