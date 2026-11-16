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

    @classmethod
    def create(cls, amount: int) -> "Service":
        return cls(amount)

    @property
    def label(self) -> str:
        if self.total() > 0:
            return "ok"
        return "no"


def build(user: User) -> int:
    service = Service(0)
    return service.total()


def make() -> int:
    service: Service = Service.create(1)
    return service.total()


def build_service(amount: int) -> Service:
    return Service(amount)


def build_from_factory() -> int:
    service = build_service(1)
    return service.total()


def build_from_classmethod(amount: int) -> int:
    service = Service.create(amount)
    return service.total()
