# typed: strict
# frozen_string_literal: true

module Models
  class Permission < T::Enum
    enums do
      Read = new
      Write = new
      Admin = new
    end
  end
end
