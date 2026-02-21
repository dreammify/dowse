# typed: strict
# frozen_string_literal: true

module Models
  class Address < T::Struct
    const :street, String
    const :city, String
    const :postal_code, String
    const :country, String
  end
end
