"""Imported bases, inherited receivers, and typed receivers through a base."""

from .models import User


class Admin(User):
    def shout(self) -> str:
        return self.greet() + self.identify()


class Staff(User):
    pass


def describe_admin(admin: Admin) -> str:
    return admin.greet()


def describe_optional(staff: Staff | None) -> str:
    return staff.identify()


def describe_forward(user: "User") -> str:
    return user.greet()


def make_admin() -> Admin:
    return Admin("root")


def via_factory() -> str:
    admin = make_admin()
    return admin.greet()
